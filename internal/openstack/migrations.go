/*
SPDX-FileCopyrightText: Copyright 2025 SAP SE or an SAP affiliate company and cobaltcore-dev contributors
SPDX-License-Identifier: Apache-2.0

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package openstack

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/gophercloud/gophercloud/v2"
)

// IncomingMigrationsLookbackWindow defines how far back we query Nova for
// migrations. Nova's live-migration monitor updates Migration.updated_at every
// ~5 seconds on both libvirt/kvm and libvirt/cloud-hypervisor backends, so any
// in-flight migration is guaranteed to appear in this window.
const IncomingMigrationsLookbackWindow = 6 * time.Hour

// MigrationInfo holds the subset of Nova migration fields relevant to settling
// incoming migrations on a host being drained.
type MigrationInfo struct {
	ID            int    `json:"id"`
	UUID          string `json:"uuid"`
	InstanceUUID  string `json:"instance_uuid"`
	Status        string `json:"status"`
	SourceCompute string `json:"source_compute"`
	DestCompute   string `json:"dest_compute"`
	MigrationType string `json:"migration_type"`
}

// migrationsResponse mirrors Nova's GET /os-migrations JSON envelope.
type migrationsResponse struct {
	Migrations []MigrationInfo `json:"migrations"`
}

// terminalStatuses are migration statuses that no longer represent an in-flight
// migration. Derived from nova/db/main/api.py migration_get_in_progress_by_host_and_node.
var terminalStatuses = map[string]bool{
	"confirmed": true,
	"reverted":  true,
	"error":     true,
	"failed":    true,
	"completed": true,
	"cancelled": true,
	"done":      true,
}

// abortableStatuses are migration statuses where a DELETE (abort) is supported.
// queued and preparing require microversion >= 2.65; running is always abortable.
// The operator uses microversion 2.90 which covers all three.
var abortableStatuses = map[string]bool{
	"queued":    true,
	"preparing": true,
	"running":   true,
}

// ListActiveIncomingMigrations queries Nova for migrations that target the
// given host and are still in a non-terminal state. It issues a single call
// with a changes-since window of IncomingMigrationsLookbackWindow, then
// filters client-side for dest_compute==host and non-terminal status.
func ListActiveIncomingMigrations(ctx context.Context, sc *gophercloud.ServiceClient, host string) ([]MigrationInfo, error) {
	changesSince := time.Now().UTC().Add(-IncomingMigrationsLookbackWindow).Format(time.RFC3339)

	migrations, err := listMigrationsForHost(ctx, sc, host, changesSince)
	if err != nil {
		return nil, fmt.Errorf("listing migrations for host %s: %w", host, err)
	}

	var result []MigrationInfo
	for _, m := range migrations {
		if m.DestCompute != host {
			continue
		}
		if terminalStatuses[m.Status] {
			continue
		}
		result = append(result, m)
	}

	return result, nil
}

// AbortMigration issues DELETE /servers/{instanceUUID}/migrations/{migrationID}
// to abort a live-migration. Treats HTTP 404 and 409 as non-fatal (the
// migration already transitioned past an abortable state or was deleted).
func AbortMigration(ctx context.Context, sc *gophercloud.ServiceClient, instanceUUID string, migrationID int) error {
	deleteURL := sc.ServiceURL("servers", instanceUUID, "migrations", strconv.Itoa(migrationID))

	resp, err := sc.Delete(ctx, deleteURL, &gophercloud.RequestOpts{
		OkCodes: []int{http.StatusAccepted, http.StatusNoContent},
	})
	if err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) || gophercloud.ResponseCodeIs(err, http.StatusConflict) {
			// Migration already completed/cancelled or not found — non-fatal.
			return nil
		}
		return fmt.Errorf("aborting migration %d for instance %s: %w", migrationID, instanceUUID, err)
	}
	// gophercloud closes the response body when JSONResponse is nil,
	// but close defensively if it wasn't.
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}
	return nil
}

// SettleIncomingMigrations combines listing and aborting: it lists all active
// incoming migrations for the host, aborts those in abortable states, and
// returns both the aborted and the waiting (post-migrating) sets.
//
// The caller should treat:
//   - len(aborted) > 0 as "just issued aborts, recheck on next reconcile"
//   - len(waiting) > 0 as "cannot abort, must wait for completion"
//   - both empty as "no incoming migrations; host is settled"
func SettleIncomingMigrations(ctx context.Context, sc *gophercloud.ServiceClient, host string) (aborted, waiting []MigrationInfo, err error) {
	active, err := ListActiveIncomingMigrations(ctx, sc, host)
	if err != nil {
		return nil, nil, err
	}

	for _, m := range active {
		if abortableStatuses[m.Status] && m.MigrationType == "live-migration" {
			if abortErr := AbortMigration(ctx, sc, m.InstanceUUID, m.ID); abortErr != nil {
				return aborted, waiting, abortErr
			}
			aborted = append(aborted, m)
		} else {
			// Non-abortable: either post-migrating, or an evacuation (which Nova
			// does not support aborting). Must wait for completion.
			waiting = append(waiting, m)
		}
	}

	return aborted, waiting, nil
}

// listMigrationsForHost queries GET /os-migrations with host and changes-since filters.
func listMigrationsForHost(ctx context.Context, sc *gophercloud.ServiceClient, host, changesSince string) ([]MigrationInfo, error) {
	query := url.Values{}
	query.Set("host", host)
	query.Set("changes-since", changesSince)

	requestURL := sc.ServiceURL("os-migrations") + "?" + query.Encode()

	var parsed migrationsResponse
	//nolint:bodyclose // gophercloud closes the body when JSONResponse is non-nil
	_, err := sc.Get(ctx, requestURL, &parsed, &gophercloud.RequestOpts{
		OkCodes: []int{http.StatusOK},
	})
	if err != nil {
		return nil, err
	}

	return parsed.Migrations, nil
}
