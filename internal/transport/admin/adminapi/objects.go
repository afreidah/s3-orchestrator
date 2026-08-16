// -------------------------------------------------------------------------------
// Admin API - Shared Object-Listing DTOs
//
// Author: Alex Freidah
//
// Wire types shared by the admin objects handler and its clients (the TUI
// browser). Kept in a leaf package so both the server and out-of-process
// clients depend on one definition and the JSON shape cannot drift.
// -------------------------------------------------------------------------------

// Package adminapi holds the wire types shared between the admin API handlers
// and their out-of-process clients.
package adminapi

// ObjectListResponse is one delimiter-grouped page of the object namespace:
// child directories collapsed into CommonPrefixes and leaf objects in Objects,
// with a continuation token when the page truncates.
type ObjectListResponse struct {
	CommonPrefixes []string      `json:"common_prefixes"`
	Objects        []ObjectEntry `json:"objects"`
	Truncated      bool          `json:"truncated"`
	Next           string        `json:"next,omitempty"`
}

// ObjectEntry is one leaf object in a listing page.
type ObjectEntry struct {
	Key  string `json:"key"`
	Size int64  `json:"size"`
}

// ObjectUploadResponse reports the ETag a stored object was recorded under.
type ObjectUploadResponse struct {
	ETag string `json:"etag"`
}

// ObjectDeleteResponse reports how many objects a delete removed. A single-key
// delete reports one; a prefix delete reports the whole count, with Failed and
// Total present when some copies could not be removed.
type ObjectDeleteResponse struct {
	Deleted int `json:"deleted"`
	Failed  int `json:"failed,omitempty"`
	Total   int `json:"total,omitempty"`
}
