// -------------------------------------------------------------------------------
// Admin API - Shared Object-Listing DTOs
//
// Author: Alex Freidah
//
// Wire types shared by the admin objects handler and its clients (the TUI
// browser). Kept in a package both the server and out-of-process clients
// import, so they depend on one definition and the JSON shape cannot drift.
// -------------------------------------------------------------------------------

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

// ObjectDeleteResponse reports a single-key delete, which removes one object.
type ObjectDeleteResponse struct {
	Deleted int `json:"deleted"`
}

// PrefixDeleteResponse reports a prefix delete: the objects it removed, the
// ones it could not, and every key it attempted. A partial removal still ran,
// so it reports ok with Failed set rather than failing the request.
type PrefixDeleteResponse struct {
	Outcome
	Deleted int `json:"deleted"`
	Failed  int `json:"failed"`
	Total   int `json:"total"`
}
