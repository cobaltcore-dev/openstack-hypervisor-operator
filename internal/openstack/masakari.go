/*
SPDX-FileCopyrightText: Copyright 2024 SAP SE or an SAP affiliate company and cobaltcore-dev contributors
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
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/pagination"
)

func getSegmentsURL(client *gophercloud.ServiceClient) string {
	return client.ServiceURL("v1", "segments")
}

func getSegmentsHostsURL(client *gophercloud.ServiceClient, segmentID string) string {
	return client.ServiceURL("v1", "segments", segmentID, "hosts")
}

func getSegmentsHostURL(client *gophercloud.ServiceClient, segmentID, hostID string) string {
	return client.ServiceURL("v1", "segments", segmentID, "hosts", hostID)
}

type MasakariPage struct {
	pagination.MarkerPageBase
}

type SegmentPage struct {
	MasakariPage
}

type MasakariTime time.Time

type Segment struct {
	UUID           string        `json:"uuid"`
	Deleted        bool          `json:"deleted"`
	CreatedAt      MasakariTime  `json:"created_at"`
	Description    string        `json:"description"`
	RecoveryMethod string        `json:"recovery_method"`
	UpdatedAt      *MasakariTime `json:"updated_at,omitempty"`
	ServiceType    string        `json:"service_type"`
	DeletedAt      *MasakariTime `json:"deleted_at,omitempty"`
	ID             int           `json:"id"`
	Name           string        `json:"name"`
	Enabled        bool          `json:"enabled"`
}

type SegmentHost struct {
	Name              string        `json:"name"`
	UUID              string        `json:"uuid"`
	FailoverSegmentId string        `json:"failover_segment_id"`
	Deleted           bool          `json:"deleted"`
	OnMaintenance     bool          `json:"on_maintenance"`
	Reserved          bool          `json:"reserved"`
	CreatedAt         MasakariTime  `json:"created_at"`
	ControlAttributes string        `json:"control_attributes"`
	UpdatedAt         *MasakariTime `json:"updated_at,omitempty"`
	Type              string        `json:"ype"`
	DeletedAt         *MasakariTime `json:"deleted_at,omitempty"`
	ID                int           `json:"id"`
	FailoverSegment   Segment       `json:"failover_segment"`
}

type SegmentHostsPage struct {
	MasakariPage
}

func (c *MasakariTime) UnmarshalJSON(b []byte) error {
	value := strings.Trim(string(b), `"`)
	if value == "" || value == "null" {
		return nil
	}
	if !strings.Contains(value, "Z") {
		var sb strings.Builder
		_, err := sb.WriteString(value)
		if err != nil {
			return err
		}

		_, err = sb.WriteString("Z")
		if err != nil {
			return err
		}

		value = sb.String()
	}
	t, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return err
	}
	*c = MasakariTime(t)
	return nil
}

func (c MasakariTime) MarshalJSON() ([]byte, error) {
	return []byte(`"` + time.Time(c).Format(time.RFC3339Nano) + `"`), nil
}

const (
	invalidMarker = "-1"
)

// NextPageURL generates the URL for the page of results after this one.
func (r MasakariPage) NextPageURL() (string, error) {
	currentURL := r.URL
	if r.Owner == nil {
		return "", nil
	}
	mark, err := r.Owner.LastMarker()
	if err != nil {
		return "", err
	}
	if mark == invalidMarker {
		return "", nil
	}

	q := currentURL.Query()
	q.Set("marker", mark)
	currentURL.RawQuery = q.Encode()
	return currentURL.String(), nil
}

// LastMarker returns the last offset in a ListResult.
func (r MasakariPage) LastMarker() (string, error) {
	shares, err := ExtractSegments(r)
	if err != nil {
		return invalidMarker, err
	}
	if len(shares) == 0 {
		return invalidMarker, nil
	}

	u, err := url.Parse(r.URL.String())
	if err != nil {
		return invalidMarker, err
	}
	queryParams := u.Query()
	marker := queryParams.Get("marker")
	limit := queryParams.Get("limit")

	// Limit is not present, only one page required
	if limit == "" {
		return invalidMarker, nil
	}

	iOffset := 0
	if marker != "" {
		iOffset, err = strconv.Atoi(marker)
		if err != nil {
			return invalidMarker, err
		}
	}
	iLimit, err := strconv.Atoi(limit)
	if err != nil {
		return invalidMarker, err
	}
	iOffset = iOffset + iLimit
	marker = strconv.Itoa(iOffset)

	return marker, nil
}

// IsEmpty satisfies the IsEmpty method of the Page interface
func (r SegmentPage) IsEmpty() (bool, error) {
	if r.StatusCode == 204 {
		return true, nil
	}

	shares, err := ExtractSegments(r)
	return len(shares) == 0, err
}

// ExtractNodes interprets the results of a single page from a List() call,
// producing a slice of Node entities.
func ExtractSegments(r pagination.Page) ([]Segment, error) {
	var s []Segment
	err := ExtractSegmentsInto(r, &s)
	return s, err
}

func ExtractSegmentsInto(r pagination.Page, v any) error {
	return r.(SegmentPage).Result.ExtractIntoSlicePtr(v, "segments")
}

// List makes a request against the API to list nodes accessible to you.
func ListSegments(client *gophercloud.ServiceClient) pagination.Pager {
	url := getSegmentsURL(client)

	return pagination.NewPager(client, url, func(r pagination.PageResult) pagination.Page {
		return SegmentPage{MasakariPage{pagination.MarkerPageBase{PageResult: r}}}
	})
}

// IsEmpty satisfies the IsEmpty method of the Page interface
func (r SegmentHostsPage) IsEmpty() (bool, error) {
	if r.StatusCode == 204 {
		return true, nil
	}

	shares, err := ExtractSegmentHosts(r)
	return len(shares) == 0, err
}

// ExtractNodes interprets the results of a single page from a List() call,
// producing a slice of Node entities.
func ExtractSegmentHosts(r pagination.Page) ([]SegmentHost, error) {
	var s []SegmentHost
	err := ExtractSegmentHostsInto(r, &s)
	return s, err
}

func ExtractSegmentHostsInto(r pagination.Page, v any) error {
	return r.(SegmentHostsPage).Result.ExtractIntoSlicePtr(v, "hosts")
}

// List makes a request against the API to list nodes accessible to you.
func ListSegmentHosts(ctx context.Context, client *gophercloud.ServiceClient, segmentID string) pagination.Pager {
	url := getSegmentsHostsURL(client, segmentID)

	return pagination.NewPager(client, url, func(r pagination.PageResult) pagination.Page {
		return SegmentHostsPage{MasakariPage{pagination.MarkerPageBase{PageResult: r}}}
	})
}

// CreateSegmentResult is the response of a Post segments operations. Call its Extract method
// to interpret it as a Segment.
type CreateSegmentResult struct {
	gophercloud.Result
}

// CreateSegmentOptsBuilder allows extensions to add additional parameters to the
// CreateSegment request.
type CreateSegmentOptsBuilder interface {
	ToMap() (map[string]any, error)
}

// CreateSegmentOps represents options used to create a resource provider.
type CreateSegmentOpts struct {
	Description    *string `json:"description,omitempty"`
	Name           string  `json:"name"`
	RecoveryMethod string  `json:"recovery_method"`
	ServiceType    string  `json:"service_type"`
	Enabled        *bool   `json:"enabled,omitempty"`
}

// ToResourceProviderUpdateTraitsMap constructs a request body from CreateSegmentOpts.
func (opts CreateSegmentOpts) ToMap() (map[string]any, error) {
	b, err := gophercloud.BuildRequestBody(opts, "segment")
	if err != nil {
		return nil, err
	}

	return b, nil
}

func CreateSegment(ctx context.Context, client *gophercloud.ServiceClient, opts CreateSegmentOpts) (r CreateSegmentResult) {
	b, err := opts.ToMap()
	if err != nil {
		r.Err = err
		return
	}
	resp, err := client.Post(ctx, getSegmentsURL(client), b, &r.Body, &gophercloud.RequestOpts{
		OkCodes: []int{200},
	})
	_, r.Header, r.Err = gophercloud.ParseResponse(resp, err)
	return
}

func (r CreateSegmentResult) Extract() (*Segment, error) {
	var s struct {
		Segment *Segment `json:"segment"`
	}
	err := r.ExtractInto(&s)
	return s.Segment, err
}

// SegmentHostResult is the response of a Post segments operations. Call its Extract method
// to interpret it as a SegmentHost.
type SegmentHostResult struct {
	gophercloud.Result
}

func (r SegmentHostResult) Extract() (*SegmentHost, error) {
	var s struct {
		Host *SegmentHost `json:"host"`
	}
	err := r.ExtractInto(&s)
	return s.Host, err
}

// CreateSegmentHostOptsBuilder allows extensions to add additional parameters to the
// CreateSegmentHost request.
type CreateSegmentHostOptsBuilder interface {
	ToMap() (map[string]any, error)
}

// CreateSegmentHostOpts represents options used to create a resource provider.
type CreateSegmentHostOpts struct {
	Type              string `json:"type"`
	Name              string `json:"name"`
	ControlAttributes string `json:"control_attributes"`
	Reserved          *bool  `json:"reserved,omitempty"`
	OnMaintenance     *bool  `json:"on_maintenance,omitempty"`
}

// ToMap constructs a request body from CreateSegmentHostOpts.
func (opts CreateSegmentHostOpts) ToMap() (map[string]any, error) {
	b, err := gophercloud.BuildRequestBody(opts, "host")
	if err != nil {
		return nil, err
	}

	return b, nil
}

func CreateSegmentHost(ctx context.Context, client *gophercloud.ServiceClient, segmentID string, opts CreateSegmentHostOpts) (r SegmentHostResult) {
	b, err := opts.ToMap()
	if err != nil {
		r.Err = err
		return
	}
	resp, err := client.Post(ctx, getSegmentsHostsURL(client, segmentID), b, &r.Body, &gophercloud.RequestOpts{
		OkCodes: []int{201},
	})
	_, r.Header, r.Err = gophercloud.ParseResponse(resp, err)
	return
}

func UpdateSegmentHost(ctx context.Context, client *gophercloud.ServiceClient, segmentID, hostID string, opts UpdateSegmentHostOpts) (r SegmentHostResult) {
	b, err := opts.ToMap()
	if err != nil {
		r.Err = err
		return
	}
	resp, err := client.Put(ctx, getSegmentsHostURL(client, segmentID, hostID), b, &r.Body, &gophercloud.RequestOpts{
		OkCodes: []int{200},
	})
	_, r.Header, r.Err = gophercloud.ParseResponse(resp, err)
	return
}

func DeleteSegmentHost(ctx context.Context, client *gophercloud.ServiceClient, segmentID, hostID string) error {
	resp, err := client.Delete(ctx, getSegmentsHostURL(client, segmentID, hostID), &gophercloud.RequestOpts{
		OkCodes: []int{204},
	})
	_, _, err = gophercloud.ParseResponse(resp, err)
	return err
}

// UpdateSegmentHostOptsBuilder allows extensions to add additional parameters to the
// UpdateSegmentHost request.
type UpdateSegmentHostOptsBuilder interface {
	ToMap() (map[string]any, error)
}

// UpdateSegmentHostOpts represents options used to create a resource provider.
type UpdateSegmentHostOpts struct {
	Type              *string `json:"type,omitempty"`
	Name              *string `json:"name,omitempty"`
	ControlAttributes *string `json:"control_attributes,omitempty"`
	Reserved          *bool   `json:"reserved,omitempty"`
	OnMaintenance     *bool   `json:"on_maintenance,omitempty"`
}

// ToMap constructs a request body from UpdateSegmentHostOpts.
func (opts UpdateSegmentHostOpts) ToMap() (map[string]any, error) {
	b, err := gophercloud.BuildRequestBody(opts, "host")
	if err != nil {
		return nil, err
	}

	return b, nil
}
