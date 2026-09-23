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

	"github.com/gophercloud/gophercloud/v2/testhelper"
	"github.com/gophercloud/gophercloud/v2/testhelper/client"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Migrations", func() {
	var (
		fakeServer testhelper.FakeServer
		ctx        context.Context
	)

	BeforeEach(func() {
		fakeServer = testhelper.SetupHTTP()
		ctx = context.Background()
	})

	AfterEach(func() {
		fakeServer.Teardown()
	})

	// migrationsHandler returns a handler that responds to GET /os-migrations.
	migrationsHandler := func(data string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Add("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, data)
		}
	}

	emptyMigrations := `{"migrations": []}`

	Describe("ListActiveIncomingMigrations", func() {
		Context("when there are no migrations", func() {
			BeforeEach(func() {
				fakeServer.Mux.HandleFunc("GET /os-migrations", migrationsHandler(emptyMigrations))
			})

			It("should return an empty list", func() {
				sc := client.ServiceClient(fakeServer)
				migrations, err := ListActiveIncomingMigrations(ctx, sc, "node009-bb549")
				Expect(err).NotTo(HaveOccurred())
				Expect(migrations).To(BeEmpty())
			})
		})

		Context("when there is a running migration targeting the host", func() {
			BeforeEach(func() {
				fakeServer.Mux.HandleFunc("GET /os-migrations", migrationsHandler(`{"migrations": [
					{
						"id": 42,
						"uuid": "mig-uuid-1",
						"instance_uuid": "inst-uuid-1",
						"status": "running",
						"source_compute": "node003-bb549",
						"dest_compute": "node009-bb549",
						"migration_type": "live-migration"
					}
				]}`))
			})

			It("should return the migration", func() {
				sc := client.ServiceClient(fakeServer)
				migrations, err := ListActiveIncomingMigrations(ctx, sc, "node009-bb549")
				Expect(err).NotTo(HaveOccurred())
				Expect(migrations).To(HaveLen(1))
				Expect(migrations[0].ID).To(Equal(42))
				Expect(migrations[0].InstanceUUID).To(Equal("inst-uuid-1"))
				Expect(migrations[0].Status).To(Equal("running"))
				Expect(migrations[0].DestCompute).To(Equal("node009-bb549"))
			})
		})

		Context("when there is a post-migrating migration targeting the host", func() {
			BeforeEach(func() {
				fakeServer.Mux.HandleFunc("GET /os-migrations", migrationsHandler(`{"migrations": [
					{
						"id": 99,
						"uuid": "mig-uuid-2",
						"instance_uuid": "inst-uuid-2",
						"status": "post-migrating",
						"source_compute": "node003-bb549",
						"dest_compute": "node009-bb549",
						"migration_type": "live-migration"
					}
				]}`))
			})

			It("should return the migration as active incoming", func() {
				sc := client.ServiceClient(fakeServer)
				migrations, err := ListActiveIncomingMigrations(ctx, sc, "node009-bb549")
				Expect(err).NotTo(HaveOccurred())
				Expect(migrations).To(HaveLen(1))
				Expect(migrations[0].Status).To(Equal("post-migrating"))
			})
		})

		Context("when there is an outbound migration (source_compute == host)", func() {
			BeforeEach(func() {
				fakeServer.Mux.HandleFunc("GET /os-migrations", migrationsHandler(`{"migrations": [
					{
						"id": 50,
						"uuid": "mig-uuid-3",
						"instance_uuid": "inst-uuid-3",
						"status": "running",
						"source_compute": "node009-bb549",
						"dest_compute": "node005-bb549",
						"migration_type": "live-migration"
					}
				]}`))
			})

			It("should ignore outbound migrations", func() {
				sc := client.ServiceClient(fakeServer)
				migrations, err := ListActiveIncomingMigrations(ctx, sc, "node009-bb549")
				Expect(err).NotTo(HaveOccurred())
				Expect(migrations).To(BeEmpty())
			})
		})

		Context("when there is an incoming evacuation", func() {
			BeforeEach(func() {
				fakeServer.Mux.HandleFunc("GET /os-migrations", migrationsHandler(`{"migrations": [
					{
						"id": 77,
						"uuid": "mig-uuid-4",
						"instance_uuid": "inst-uuid-4",
						"status": "running",
						"source_compute": "node001-bb549",
						"dest_compute": "node009-bb549",
						"migration_type": "evacuation"
					}
				]}`))
			})

			It("should include incoming evacuations", func() {
				sc := client.ServiceClient(fakeServer)
				migrations, err := ListActiveIncomingMigrations(ctx, sc, "node009-bb549")
				Expect(err).NotTo(HaveOccurred())
				Expect(migrations).To(HaveLen(1))
				Expect(migrations[0].MigrationType).To(Equal("evacuation"))
				Expect(migrations[0].DestCompute).To(Equal("node009-bb549"))
			})
		})

		Context("when migrations have terminal statuses", func() {
			BeforeEach(func() {
				fakeServer.Mux.HandleFunc("GET /os-migrations", migrationsHandler(`{"migrations": [
					{
						"id": 10,
						"uuid": "mig-t1",
						"instance_uuid": "inst-t1",
						"status": "completed",
						"source_compute": "node003-bb549",
						"dest_compute": "node009-bb549",
						"migration_type": "live-migration"
					},
					{
						"id": 11,
						"uuid": "mig-t2",
						"instance_uuid": "inst-t2",
						"status": "cancelled",
						"source_compute": "node003-bb549",
						"dest_compute": "node009-bb549",
						"migration_type": "live-migration"
					},
					{
						"id": 12,
						"uuid": "mig-t3",
						"instance_uuid": "inst-t3",
						"status": "error",
						"source_compute": "node003-bb549",
						"dest_compute": "node009-bb549",
						"migration_type": "live-migration"
					},
					{
						"id": 13,
						"uuid": "mig-t4",
						"instance_uuid": "inst-t4",
						"status": "failed",
						"source_compute": "node003-bb549",
						"dest_compute": "node009-bb549",
						"migration_type": "live-migration"
					},
					{
						"id": 14,
						"uuid": "mig-t5",
						"instance_uuid": "inst-t5",
						"status": "confirmed",
						"source_compute": "node003-bb549",
						"dest_compute": "node009-bb549",
						"migration_type": "live-migration"
					},
					{
						"id": 15,
						"uuid": "mig-t6",
						"instance_uuid": "inst-t6",
						"status": "reverted",
						"source_compute": "node003-bb549",
						"dest_compute": "node009-bb549",
						"migration_type": "live-migration"
					},
					{
						"id": 16,
						"uuid": "mig-t7",
						"instance_uuid": "inst-t7",
						"status": "done",
						"source_compute": "node003-bb549",
						"dest_compute": "node009-bb549",
						"migration_type": "live-migration"
					}
				]}`))
			})

			It("should filter out all terminal statuses", func() {
				sc := client.ServiceClient(fakeServer)
				migrations, err := ListActiveIncomingMigrations(ctx, sc, "node009-bb549")
				Expect(err).NotTo(HaveOccurred())
				Expect(migrations).To(BeEmpty())
			})
		})

		Context("when Nova returns a server error", func() {
			BeforeEach(func() {
				fakeServer.Mux.HandleFunc("GET /os-migrations", func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(http.StatusInternalServerError)
					fmt.Fprint(w, `{"error": "Internal Server Error"}`)
				})
			})

			It("should propagate the error", func() {
				sc := client.ServiceClient(fakeServer)
				_, err := ListActiveIncomingMigrations(ctx, sc, "node009-bb549")
				Expect(err).To(HaveOccurred())
			})
		})

		Context("with a mixed set of migrations", func() {
			BeforeEach(func() {
				fakeServer.Mux.HandleFunc("GET /os-migrations", migrationsHandler(`{"migrations": [
					{
						"id": 1,
						"uuid": "mig-1",
						"instance_uuid": "inst-1",
						"status": "running",
						"source_compute": "node003-bb549",
						"dest_compute": "node009-bb549",
						"migration_type": "live-migration"
					},
					{
						"id": 2,
						"uuid": "mig-2",
						"instance_uuid": "inst-2",
						"status": "running",
						"source_compute": "node007-bb549",
						"dest_compute": "node009-bb549",
						"migration_type": "live-migration"
					},
					{
						"id": 3,
						"uuid": "mig-3",
						"instance_uuid": "inst-3",
						"status": "post-migrating",
						"source_compute": "node002-bb549",
						"dest_compute": "node009-bb549",
						"migration_type": "live-migration"
					},
					{
						"id": 4,
						"uuid": "mig-4",
						"instance_uuid": "inst-4",
						"status": "running",
						"source_compute": "node009-bb549",
						"dest_compute": "node005-bb549",
						"migration_type": "live-migration"
					},
					{
						"id": 5,
						"uuid": "mig-5",
						"instance_uuid": "inst-5",
						"status": "completed",
						"source_compute": "node003-bb549",
						"dest_compute": "node009-bb549",
						"migration_type": "live-migration"
					}
				]}`))
			})

			It("should return only active incoming migrations", func() {
				sc := client.ServiceClient(fakeServer)
				migrations, err := ListActiveIncomingMigrations(ctx, sc, "node009-bb549")
				Expect(err).NotTo(HaveOccurred())
				Expect(migrations).To(HaveLen(3))
				ids := []int{migrations[0].ID, migrations[1].ID, migrations[2].ID}
				Expect(ids).To(ConsistOf(1, 2, 3))
			})
		})

		Context("verifies query parameters", func() {
			var receivedQueries []string

			BeforeEach(func() {
				receivedQueries = nil
				fakeServer.Mux.HandleFunc("GET /os-migrations", func(w http.ResponseWriter, r *http.Request) {
					receivedQueries = append(receivedQueries, r.URL.RawQuery)
					w.Header().Add("Content-Type", "application/json")
					w.WriteHeader(http.StatusOK)
					fmt.Fprint(w, `{"migrations": []}`)
				})
			})

			It("should send host and changes-since parameters", func() {
				sc := client.ServiceClient(fakeServer)
				_, err := ListActiveIncomingMigrations(ctx, sc, "node009-bb549")
				Expect(err).NotTo(HaveOccurred())
				// Single call without migration_type filter
				Expect(receivedQueries).To(HaveLen(1))
				Expect(receivedQueries[0]).To(ContainSubstring("host=node009-bb549"))
				Expect(receivedQueries[0]).To(ContainSubstring("changes-since="))
				Expect(receivedQueries[0]).NotTo(ContainSubstring("migration_type"))
			})
		})
	})

	Describe("AbortMigration", func() {
		Context("when abort succeeds", func() {
			var deleteCalled bool

			BeforeEach(func() {
				deleteCalled = false
				fakeServer.Mux.HandleFunc("DELETE /servers/inst-uuid-1/migrations/42", func(w http.ResponseWriter, r *http.Request) {
					deleteCalled = true
					w.WriteHeader(http.StatusAccepted)
				})
			})

			It("should issue DELETE and return nil", func() {
				sc := client.ServiceClient(fakeServer)
				err := AbortMigration(ctx, sc, "inst-uuid-1", 42)
				Expect(err).NotTo(HaveOccurred())
				Expect(deleteCalled).To(BeTrue())
			})
		})

		Context("when Nova returns 404 (migration already gone)", func() {
			BeforeEach(func() {
				fakeServer.Mux.HandleFunc("DELETE /servers/inst-uuid-1/migrations/42", func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(http.StatusNotFound)
					fmt.Fprint(w, `{"itemNotFound": {"message": "Migration not found", "code": 404}}`)
				})
			})

			It("should treat as non-fatal", func() {
				sc := client.ServiceClient(fakeServer)
				err := AbortMigration(ctx, sc, "inst-uuid-1", 42)
				Expect(err).NotTo(HaveOccurred())
			})
		})

		Context("when Nova returns 409 (migration not abortable)", func() {
			BeforeEach(func() {
				fakeServer.Mux.HandleFunc("DELETE /servers/inst-uuid-1/migrations/42", func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(http.StatusConflict)
					fmt.Fprint(w, `{"conflictingRequest": {"message": "Migration status is post-migrating", "code": 409}}`)
				})
			})

			It("should treat as non-fatal", func() {
				sc := client.ServiceClient(fakeServer)
				err := AbortMigration(ctx, sc, "inst-uuid-1", 42)
				Expect(err).NotTo(HaveOccurred())
			})
		})

		Context("when Nova returns a server error", func() {
			BeforeEach(func() {
				fakeServer.Mux.HandleFunc("DELETE /servers/inst-uuid-1/migrations/42", func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(http.StatusInternalServerError)
					fmt.Fprint(w, `{"error": "Internal Server Error"}`)
				})
			})

			It("should propagate the error", func() {
				sc := client.ServiceClient(fakeServer)
				err := AbortMigration(ctx, sc, "inst-uuid-1", 42)
				Expect(err).To(HaveOccurred())
			})
		})
	})

	Describe("SettleIncomingMigrations", func() {
		Context("when there are no incoming migrations", func() {
			BeforeEach(func() {
				fakeServer.Mux.HandleFunc("GET /os-migrations", migrationsHandler(emptyMigrations))
			})

			It("should return empty aborted and waiting", func() {
				sc := client.ServiceClient(fakeServer)
				aborted, waiting, err := SettleIncomingMigrations(ctx, sc, "node009-bb549")
				Expect(err).NotTo(HaveOccurred())
				Expect(aborted).To(BeEmpty())
				Expect(waiting).To(BeEmpty())
			})
		})

		Context("when there are abortable migrations", func() {
			var deleteCalls int

			BeforeEach(func() {
				deleteCalls = 0
				fakeServer.Mux.HandleFunc("GET /os-migrations", migrationsHandler(`{"migrations": [
					{
						"id": 1,
						"uuid": "mig-1",
						"instance_uuid": "inst-1",
						"status": "running",
						"source_compute": "node003-bb549",
						"dest_compute": "node009-bb549",
						"migration_type": "live-migration"
					},
					{
						"id": 2,
						"uuid": "mig-2",
						"instance_uuid": "inst-2",
						"status": "queued",
						"source_compute": "node007-bb549",
						"dest_compute": "node009-bb549",
						"migration_type": "live-migration"
					}
				]}`))

				fakeServer.Mux.HandleFunc("DELETE /servers/inst-1/migrations/1", func(w http.ResponseWriter, r *http.Request) {
					deleteCalls++
					w.WriteHeader(http.StatusAccepted)
				})

				fakeServer.Mux.HandleFunc("DELETE /servers/inst-2/migrations/2", func(w http.ResponseWriter, r *http.Request) {
					deleteCalls++
					w.WriteHeader(http.StatusAccepted)
				})
			})

			It("should abort all and return them in aborted list", func() {
				sc := client.ServiceClient(fakeServer)
				aborted, waiting, err := SettleIncomingMigrations(ctx, sc, "node009-bb549")
				Expect(err).NotTo(HaveOccurred())
				Expect(aborted).To(HaveLen(2))
				Expect(waiting).To(BeEmpty())
				Expect(deleteCalls).To(Equal(2))
			})
		})

		Context("when there is a post-migrating migration (not abortable)", func() {
			BeforeEach(func() {
				fakeServer.Mux.HandleFunc("GET /os-migrations", migrationsHandler(`{"migrations": [
					{
						"id": 99,
						"uuid": "mig-99",
						"instance_uuid": "inst-99",
						"status": "post-migrating",
						"source_compute": "node003-bb549",
						"dest_compute": "node009-bb549",
						"migration_type": "live-migration"
					}
				]}`))
			})

			It("should return in waiting list without issuing DELETE", func() {
				sc := client.ServiceClient(fakeServer)
				aborted, waiting, err := SettleIncomingMigrations(ctx, sc, "node009-bb549")
				Expect(err).NotTo(HaveOccurred())
				Expect(aborted).To(BeEmpty())
				Expect(waiting).To(HaveLen(1))
				Expect(waiting[0].ID).To(Equal(99))
			})
		})

		Context("with a mixed set (abortable + post-migrating + outbound + completed)", func() {
			var deleteCalls int

			BeforeEach(func() {
				deleteCalls = 0
				fakeServer.Mux.HandleFunc("GET /os-migrations", migrationsHandler(`{"migrations": [
					{
						"id": 1,
						"uuid": "mig-1",
						"instance_uuid": "inst-1",
						"status": "running",
						"source_compute": "node003-bb549",
						"dest_compute": "node009-bb549",
						"migration_type": "live-migration"
					},
					{
						"id": 2,
						"uuid": "mig-2",
						"instance_uuid": "inst-2",
						"status": "preparing",
						"source_compute": "node007-bb549",
						"dest_compute": "node009-bb549",
						"migration_type": "live-migration"
					},
					{
						"id": 3,
						"uuid": "mig-3",
						"instance_uuid": "inst-3",
						"status": "post-migrating",
						"source_compute": "node002-bb549",
						"dest_compute": "node009-bb549",
						"migration_type": "live-migration"
					},
					{
						"id": 4,
						"uuid": "mig-4",
						"instance_uuid": "inst-4",
						"status": "running",
						"source_compute": "node009-bb549",
						"dest_compute": "node005-bb549",
						"migration_type": "live-migration"
					},
					{
						"id": 5,
						"uuid": "mig-5",
						"instance_uuid": "inst-5",
						"status": "completed",
						"source_compute": "node003-bb549",
						"dest_compute": "node009-bb549",
						"migration_type": "live-migration"
					}
				]}`))

				fakeServer.Mux.HandleFunc("DELETE /servers/inst-1/migrations/1", func(w http.ResponseWriter, r *http.Request) {
					deleteCalls++
					w.WriteHeader(http.StatusAccepted)
				})

				fakeServer.Mux.HandleFunc("DELETE /servers/inst-2/migrations/2", func(w http.ResponseWriter, r *http.Request) {
					deleteCalls++
					w.WriteHeader(http.StatusAccepted)
				})
			})

			It("should abort 2, wait on 1, ignore outbound and completed", func() {
				sc := client.ServiceClient(fakeServer)
				aborted, waiting, err := SettleIncomingMigrations(ctx, sc, "node009-bb549")
				Expect(err).NotTo(HaveOccurred())
				Expect(aborted).To(HaveLen(2))
				Expect(waiting).To(HaveLen(1))
				Expect(waiting[0].ID).To(Equal(3))
				Expect(deleteCalls).To(Equal(2))
			})
		})

		Context("when Nova list returns an error", func() {
			BeforeEach(func() {
				fakeServer.Mux.HandleFunc("GET /os-migrations", func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(http.StatusInternalServerError)
					fmt.Fprint(w, `{"error": "Internal Server Error"}`)
				})
			})

			It("should propagate the error", func() {
				sc := client.ServiceClient(fakeServer)
				_, _, err := SettleIncomingMigrations(ctx, sc, "node009-bb549")
				Expect(err).To(HaveOccurred())
			})
		})

		Context("when there is a running evacuation (not abortable via migration-abort)", func() {
			BeforeEach(func() {
				fakeServer.Mux.HandleFunc("GET /os-migrations", migrationsHandler(`{"migrations": [
					{
						"id": 88,
						"uuid": "mig-evac-1",
						"instance_uuid": "inst-evac-1",
						"status": "running",
						"source_compute": "node001-bb549",
						"dest_compute": "node009-bb549",
						"migration_type": "evacuation"
					}
				]}`))
			})

			It("should place the evacuation in waiting without issuing DELETE", func() {
				sc := client.ServiceClient(fakeServer)
				aborted, waiting, err := SettleIncomingMigrations(ctx, sc, "node009-bb549")
				Expect(err).NotTo(HaveOccurred())
				Expect(aborted).To(BeEmpty())
				Expect(waiting).To(HaveLen(1))
				Expect(waiting[0].ID).To(Equal(88))
				Expect(waiting[0].MigrationType).To(Equal("evacuation"))
			})
		})
	})
})
