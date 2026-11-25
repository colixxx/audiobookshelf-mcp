package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// toolHandler is the signature used by every MCP tool handler in this server.
type toolHandler = func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error)

// encodeFilterValue encodes a value the way the Audiobookshelf web client does for
// filter params and name-based path ids (narrators, genres, tags): base64, then URL-escaped.
func encodeFilterValue(v string) string {
	return url.QueryEscape(base64.StdEncoding.EncodeToString([]byte(v)))
}

// absCall resolves config, performs the request and wraps the result for MCP.
func absCall(ctx context.Context, request mcp.CallToolRequest, method, path string, payload interface{}) (*mcp.CallToolResult, error) {
	baseURL, token, err := getABSConfig(request)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	body, err := absRequest(ctx, method, baseURL, token, path, payload)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if len(strings.TrimSpace(string(body))) == 0 || string(body) == "OK" {
		return mcp.NewToolResultText(`{"success":true}`), nil
	}
	return mcp.NewToolResultText(string(body)), nil
}

func hasArg(args map[string]any, key string) bool {
	_, ok := args[key]
	return ok
}

func toStringSlice(v any) ([]string, error) {
	switch t := v.(type) {
	case nil:
		return []string{}, nil
	case []string:
		return t, nil
	case []any:
		out := make([]string, 0, len(t))
		for _, x := range t {
			s, ok := x.(string)
			if !ok {
				return nil, fmt.Errorf("expected array of strings, got element %v", x)
			}
			out = append(out, s)
		}
		return out, nil
	case string:
		// Be lenient: allow comma-separated strings.
		if strings.TrimSpace(t) == "" {
			return []string{}, nil
		}
		parts := strings.Split(t, ",")
		for i := range parts {
			parts[i] = strings.TrimSpace(parts[i])
		}
		return parts, nil
	default:
		return nil, fmt.Errorf("expected array of strings, got %T", v)
	}
}

// metadata string fields: MCP argument name -> ABS metadata key
var metadataStringFields = map[string]string{
	"title":          "title",
	"subtitle":       "subtitle",
	"description":    "description",
	"published_year": "publishedYear",
	"published_date": "publishedDate",
	"publisher":      "publisher",
	"language":       "language",
	"isbn":           "isbn",
	"asin":           "asin",
}

// buildMediaPayload converts flat tool arguments into the ABS media update payload
// (PATCH /api/items/:id/media). Only arguments that are present are sent, so fields
// that are not mentioned stay untouched. An empty string clears a string field,
// an empty array clears a list field.
func buildMediaPayload(args map[string]any) (map[string]any, error) {
	metadata := map[string]any{}
	payload := map[string]any{}

	for arg, key := range metadataStringFields {
		if !hasArg(args, arg) {
			continue
		}
		switch v := args[arg].(type) {
		case nil:
			metadata[key] = nil
		case string:
			metadata[key] = v
		case float64:
			metadata[key] = strconv.FormatFloat(v, 'f', -1, 64)
		default:
			return nil, fmt.Errorf("%s must be a string", arg)
		}
	}

	for _, arg := range []string{"explicit", "abridged"} {
		if hasArg(args, arg) {
			b, ok := args[arg].(bool)
			if !ok {
				return nil, fmt.Errorf("%s must be a boolean", arg)
			}
			metadata[arg] = b
		}
	}

	for _, arg := range []string{"narrators", "genres"} {
		if hasArg(args, arg) {
			list, err := toStringSlice(args[arg])
			if err != nil {
				return nil, fmt.Errorf("%s: %w", arg, err)
			}
			metadata[arg] = list
		}
	}

	if hasArg(args, "authors") {
		names, err := toStringSlice(args["authors"])
		if err != nil {
			return nil, fmt.Errorf("authors: %w", err)
		}
		authors := make([]map[string]any, 0, len(names))
		for _, n := range names {
			if n = strings.TrimSpace(n); n != "" {
				authors = append(authors, map[string]any{"name": n})
			}
		}
		metadata["authors"] = authors
	}

	if hasArg(args, "series") {
		raw, ok := args["series"].([]any)
		if !ok && args["series"] != nil {
			return nil, fmt.Errorf("series must be an array of {name, sequence} objects")
		}
		series := make([]map[string]any, 0, len(raw))
		for _, x := range raw {
			obj, ok := x.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("series must be an array of {name, sequence} objects")
			}
			name, _ := obj["name"].(string)
			name = strings.TrimSpace(name)
			if name == "" {
				return nil, fmt.Errorf("every series entry needs a non-empty name")
			}
			entry := map[string]any{"name": name}
			switch seq := obj["sequence"].(type) {
			case string:
				if seq != "" {
					entry["sequence"] = seq
				}
			case float64:
				entry["sequence"] = strconv.FormatFloat(seq, 'f', -1, 64)
			case nil:
			default:
				return nil, fmt.Errorf("series sequence must be a string like \"13.2\"")
			}
			series = append(series, entry)
		}
		metadata["series"] = series
	}

	if hasArg(args, "tags") {
		list, err := toStringSlice(args["tags"])
		if err != nil {
			return nil, fmt.Errorf("tags: %w", err)
		}
		payload["tags"] = list
	}

	if len(metadata) > 0 {
		payload["metadata"] = metadata
	}
	return payload, nil
}

// mediaFieldOptions are the editable metadata fields shared by update_item and batch_update_items.
func mediaFieldOptions() []mcp.ToolOption {
	return []mcp.ToolOption{
		mcp.WithString("title", mcp.Description("Book title")),
		mcp.WithString("subtitle", mcp.Description("Subtitle")),
		mcp.WithArray("authors", mcp.WithStringItems(), mcp.Description("Full list of author names (replaces current authors). Existing authors with the same name are reused")),
		mcp.WithArray("narrators", mcp.WithStringItems(), mcp.Description("Full list of narrator names (replaces current narrators)")),
		mcp.WithArray("series", mcp.Description("Full list of series (replaces current series), e.g. [{\"name\":\"Файролл\",\"sequence\":\"13.2\"}]. Empty array removes the book from all series"),
			mcp.Items(map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name":     map[string]any{"type": "string"},
					"sequence": map[string]any{"type": "string"},
				},
				"required": []string{"name"},
			})),
		mcp.WithArray("genres", mcp.WithStringItems(), mcp.Description("Full list of genres (replaces current genres)")),
		mcp.WithArray("tags", mcp.WithStringItems(), mcp.Description("Full list of tags (replaces current tags)")),
		mcp.WithString("description", mcp.Description("Description (HTML allowed, will be sanitized)")),
		mcp.WithString("published_year", mcp.Description("Published year, e.g. \"2021\"")),
		mcp.WithString("published_date", mcp.Description("Published date")),
		mcp.WithString("publisher", mcp.Description("Publisher")),
		mcp.WithString("language", mcp.Description("Language")),
		mcp.WithString("isbn", mcp.Description("ISBN")),
		mcp.WithString("asin", mcp.Description("ASIN")),
		mcp.WithBoolean("explicit", mcp.Description("Explicit flag")),
		mcp.WithBoolean("abridged", mcp.Description("Abridged flag")),
	}
}

func opts(description string, extra ...mcp.ToolOption) []mcp.ToolOption {
	o := append(withABSAuth(), mcp.WithDescription(description))
	return append(o, extra...)
}

// libraryHandler replaces the generic sub-resource handler for the `library` tool so that
// query parameters (search query, pagination, sorting, filters) can be passed through.
func libraryHandler() toolHandler {
	subResources := []string{
		"items", "authors", "series", "narrators", "collections", "playlists", "personalized",
		"filterdata", "stats", "search", "episode-downloads", "recent-episodes",
	}
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id, err := request.RequireString("library_id")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		path := fmt.Sprintf("/libraries/%s", id)
		sub := ""
		for _, s := range subResources {
			if request.GetBool(s, false) {
				sub = s
				path += "/" + s
				break
			}
		}

		q := url.Values{}
		args := request.GetArguments()
		if query := request.GetString("q", ""); query != "" {
			q.Set("q", query)
			if sub == "" {
				sub = "search"
				path += "/search"
			}
		}
		if sub == "search" && q.Get("q") == "" {
			return mcp.NewToolResultError("search requires the q parameter"), nil
		}
		if hasArg(args, "limit") {
			q.Set("limit", strconv.Itoa(request.GetInt("limit", 0)))
		} else if sub == "series" {
			// ABS turns a missing limit into LIMIT 0 for series and returns an empty list.
			q.Set("limit", "1000")
		}
		if hasArg(args, "page") {
			q.Set("page", strconv.Itoa(request.GetInt("page", 0)))
		} else if q.Get("limit") != "" {
			q.Set("page", "0")
		}
		if sort := request.GetString("sort", ""); sort != "" {
			q.Set("sort", sort)
		}
		if request.GetBool("desc", false) {
			q.Set("desc", "1")
		}
		if group := request.GetString("filter_group", ""); group != "" {
			val := request.GetString("filter_value", "")
			if val == "" {
				q.Set("filter", group)
			} else {
				q.Set("filter", group+"."+base64.StdEncoding.EncodeToString([]byte(val)))
			}
		}
		if request.GetBool("minified", false) {
			q.Set("minified", "1")
		}
		if request.GetBool("collapse_series", false) {
			q.Set("collapseseries", "1")
		}
		if include := request.GetString("include", ""); include != "" {
			q.Set("include", include)
		}
		if len(q) > 0 {
			path += "?" + q.Encode()
		}
		return absCall(ctx, request, "GET", path, nil)
	}
}

func libraryToolOptions() []mcp.ToolOption {
	return opts("Retrieve a single Audiobookshelf library by ID, optionally with a sub-resource. List sub-resources (items, series, authors) accept pagination, sorting and filters",
		mcp.WithString("library_id", mcp.Required(), mcp.Description("Library identifier to fetch")),
		mcp.WithBoolean("items", mcp.Description("List library items (use limit/page/sort/filter/minified)")),
		mcp.WithBoolean("authors", mcp.Description("List authors in the library")),
		mcp.WithBoolean("series", mcp.Description("List series in the library (books included)")),
		mcp.WithBoolean("narrators", mcp.Description("List narrators in the library with book counts")),
		mcp.WithBoolean("collections", mcp.Description("List collections in the library")),
		mcp.WithBoolean("playlists", mcp.Description("List playlists in the library")),
		mcp.WithBoolean("personalized", mcp.Description("Personalized shelves for the library")),
		mcp.WithBoolean("filterdata", mcp.Description("Filter data (authors, series, genres, tags, narrators, languages)")),
		mcp.WithBoolean("stats", mcp.Description("Library statistics")),
		mcp.WithBoolean("search", mcp.Description("Search the library (requires q)")),
		mcp.WithBoolean("episode-downloads", mcp.Description("Episode downloads for the library")),
		mcp.WithBoolean("recent-episodes", mcp.Description("Recent episodes for the library")),
		mcp.WithString("q", mcp.Description("Search query: title, author, series, narrator, tag, genre. Implies search when no other sub-resource is set")),
		mcp.WithNumber("limit", mcp.Description("Page size (series default 1000; items: 0 = all)")),
		mcp.WithNumber("page", mcp.Description("Page number, 0-based")),
		mcp.WithString("sort", mcp.Description("Sort field, e.g. media.metadata.title, media.metadata.authorName, addedAt, media.duration; for series: name, numBooks, addedAt")),
		mcp.WithBoolean("desc", mcp.Description("Sort descending")),
		mcp.WithString("filter_group", mcp.Description("Filter group: genres, tags, series, authors, narrators, publishers, languages, progress, missing, issues, feed-open")),
		mcp.WithString("filter_value", mcp.Description("Raw filter value (an id for series/authors, a name for genres/tags/narrators, e.g. 'finished' for progress); encoded automatically")),
		mcp.WithBoolean("minified", mcp.Description("Return minified items (much smaller responses)")),
		mcp.WithBoolean("collapse_series", mcp.Description("Collapse books of a series into one entry")),
		mcp.WithString("include", mcp.Description("Comma-separated includes, e.g. rssfeed,progress")),
	)
}

func registerEditTools(s *server.MCPServer) {
	// --- Items ---
	s.AddTool(mcp.NewTool("batch_get_items", opts("Get several library items (expanded) by ID in one call",
		mcp.WithArray("item_ids", mcp.Required(), mcp.WithStringItems(), mcp.Description("Library item IDs")),
	)...), func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		ids, err := request.RequireStringSlice("item_ids")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return absCall(ctx, request, "POST", "/items/batch/get", map[string]any{"libraryItemIds": ids})
	})

	updateItemOpts := append(opts("Update metadata of a library item (book): title, authors, narrators, series with sequence, genres, tags, description, etc. Only the fields you pass are changed; list fields replace the whole list. Changes are stored in the ABS database (and in metadata.json if the library has that setting); audio files are not touched",
		mcp.WithString("item_id", mcp.Required(), mcp.Description("Library item ID")),
	), mediaFieldOptions()...)
	s.AddTool(mcp.NewTool("update_item", updateItemOpts...), func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id, err := request.RequireString("item_id")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		payload, err := buildMediaPayload(request.GetArguments())
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		if len(payload) == 0 {
			return mcp.NewToolResultError("nothing to update: pass at least one metadata field"), nil
		}
		return absCall(ctx, request, "PATCH", fmt.Sprintf("/items/%s/media", id), payload)
	})

	s.AddTool(mcp.NewTool("batch_update_items", opts("Update metadata of many library items in one request. Each update is an object with item_id plus any update_item fields (title, subtitle, authors, narrators, series [{name,sequence}], genres, tags, description, published_year, publisher, language, isbn, asin, explicit, abridged)",
		mcp.WithArray("updates", mcp.Required(), mcp.Description("Array of {item_id, ...fields}"),
			mcp.Items(map[string]any{"type": "object", "properties": map[string]any{"item_id": map[string]any{"type": "string"}}, "required": []string{"item_id"}})),
	)...), func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		raw, ok := request.GetArguments()["updates"].([]any)
		if !ok || len(raw) == 0 {
			return mcp.NewToolResultError("updates must be a non-empty array"), nil
		}
		body := make([]map[string]any, 0, len(raw))
		seen := map[string]bool{}
		for i, x := range raw {
			obj, ok := x.(map[string]any)
			if !ok {
				return mcp.NewToolResultError(fmt.Sprintf("updates[%d] must be an object", i)), nil
			}
			id, _ := obj["item_id"].(string)
			if id == "" {
				return mcp.NewToolResultError(fmt.Sprintf("updates[%d].item_id is required", i)), nil
			}
			if seen[id] {
				return mcp.NewToolResultError(fmt.Sprintf("item %s appears twice; ABS requires unique ids per batch", id)), nil
			}
			seen[id] = true
			mp, err := buildMediaPayload(obj)
			if err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("updates[%d]: %v", i, err)), nil
			}
			if len(mp) == 0 {
				return mcp.NewToolResultError(fmt.Sprintf("updates[%d]: nothing to update", i)), nil
			}
			body = append(body, map[string]any{"id": id, "mediaPayload": mp})
		}
		return absCall(ctx, request, "POST", "/items/batch/update", body)
	})

	s.AddTool(mcp.NewTool("match_item", opts("Quick-match a library item against a metadata provider (e.g. the library's provider, google, openlibrary, audible, custom-<slug>). Without override flags only empty fields are filled",
		mcp.WithString("item_id", mcp.Required(), mcp.Description("Library item ID")),
		mcp.WithString("provider", mcp.Description("Metadata provider; defaults to the library's provider")),
		mcp.WithString("title", mcp.Description("Title to search for")),
		mcp.WithString("author", mcp.Description("Author to search for")),
		mcp.WithString("isbn", mcp.Description("ISBN to search for")),
		mcp.WithString("asin", mcp.Description("ASIN to search for")),
		mcp.WithBoolean("override_details", mcp.Description("Overwrite existing details with the match")),
		mcp.WithBoolean("override_cover", mcp.Description("Overwrite the existing cover with the match")),
	)...), func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id, err := request.RequireString("item_id")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		args := request.GetArguments()
		payload := map[string]any{}
		for _, k := range []string{"provider", "title", "author", "isbn", "asin"} {
			if v := request.GetString(k, ""); v != "" {
				payload[k] = v
			}
		}
		if hasArg(args, "override_details") {
			payload["overrideDetails"] = request.GetBool("override_details", false)
		}
		if hasArg(args, "override_cover") {
			payload["overrideCover"] = request.GetBool("override_cover", false)
		}
		return absCall(ctx, request, "POST", fmt.Sprintf("/items/%s/match", id), payload)
	})

	s.AddTool(mcp.NewTool("set_item_cover", opts("Set the cover of a library item, either downloaded from a URL or from an image file already inside the item folder",
		mcp.WithString("item_id", mcp.Required(), mcp.Description("Library item ID")),
		mcp.WithString("url", mcp.Description("Image URL to download")),
		mcp.WithString("path", mcp.Description("Absolute server path of an existing image file in the item folder")),
	)...), func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id, err := request.RequireString("item_id")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		u, p := request.GetString("url", ""), request.GetString("path", "")
		switch {
		case u != "" && p != "":
			return mcp.NewToolResultError("pass either url or path, not both"), nil
		case u != "":
			return absCall(ctx, request, "POST", fmt.Sprintf("/items/%s/cover", id), map[string]any{"url": u})
		case p != "":
			return absCall(ctx, request, "PATCH", fmt.Sprintf("/items/%s/cover", id), map[string]any{"cover": p})
		default:
			return mcp.NewToolResultError("url or path is required"), nil
		}
	})

	s.AddTool(mcp.NewTool("scan_item", opts("Rescan a single library item from disk",
		mcp.WithString("item_id", mcp.Required(), mcp.Description("Library item ID")),
	)...), func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id, err := request.RequireString("item_id")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return absCall(ctx, request, "POST", fmt.Sprintf("/items/%s/scan", id), nil)
	})

	s.AddTool(mcp.NewTool("delete_item", opts("Remove a library item from Audiobookshelf. By default only the database entry is removed and files stay on disk (the next scan may re-add them). With delete_files=true the item folder is DELETED FROM DISK — irreversible",
		mcp.WithString("item_id", mcp.Required(), mcp.Description("Library item ID")),
		mcp.WithBoolean("delete_files", mcp.Description("Also delete the files from disk (irreversible). Default false")),
	)...), func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id, err := request.RequireString("item_id")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		path := fmt.Sprintf("/items/%s", id)
		if request.GetBool("delete_files", false) {
			path += "?hard=1"
		}
		return absCall(ctx, request, "DELETE", path, nil)
	})

	// --- Libraries ---
	s.AddTool(mcp.NewTool("scan_library", opts("Start a scan of a library (runs in the background)",
		mcp.WithString("library_id", mcp.Required(), mcp.Description("Library ID")),
		mcp.WithBoolean("force", mcp.Description("Force rescan of all items, not only changed ones")),
	)...), func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id, err := request.RequireString("library_id")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		path := fmt.Sprintf("/libraries/%s/scan", id)
		if request.GetBool("force", false) {
			path += "?force=1"
		}
		return absCall(ctx, request, "POST", path, nil)
	})

	s.AddTool(mcp.NewTool("update_library", opts("Update library name, metadata provider or settings (e.g. {\"autoScanCronExpression\":\"*/15 * * * *\", \"disableWatcher\":false, \"metadataPrecedence\":[...]}). Only passed settings keys change",
		mcp.WithString("library_id", mcp.Required(), mcp.Description("Library ID")),
		mcp.WithString("name", mcp.Description("New library name")),
		mcp.WithString("provider", mcp.Description("Default metadata provider, e.g. google, openlibrary, custom-<slug>")),
		mcp.WithObject("settings", mcp.Description("Library settings to change")),
	)...), func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id, err := request.RequireString("library_id")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		args := request.GetArguments()
		payload := map[string]any{}
		if v := request.GetString("name", ""); v != "" {
			payload["name"] = v
		}
		if v := request.GetString("provider", ""); v != "" {
			payload["provider"] = v
		}
		if st, ok := args["settings"].(map[string]any); ok && len(st) > 0 {
			payload["settings"] = st
		}
		if len(payload) == 0 {
			return mcp.NewToolResultError("nothing to update"), nil
		}
		return absCall(ctx, request, "PATCH", fmt.Sprintf("/libraries/%s", id), payload)
	})

	// --- Authors / series / narrators ---
	s.AddTool(mcp.NewTool("update_author", opts("Update an author. Renaming to the exact name of another author in the same library MERGES the two authors (books move to the existing one)",
		mcp.WithString("author_id", mcp.Required(), mcp.Description("Author ID")),
		mcp.WithString("name", mcp.Description("New name")),
		mcp.WithString("description", mcp.Description("Description")),
		mcp.WithString("asin", mcp.Description("ASIN")),
	)...), func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id, err := request.RequireString("author_id")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		args := request.GetArguments()
		payload := map[string]any{}
		for _, k := range []string{"name", "description", "asin"} {
			if hasArg(args, k) {
				payload[k] = request.GetString(k, "")
			}
		}
		if len(payload) == 0 {
			return mcp.NewToolResultError("nothing to update"), nil
		}
		return absCall(ctx, request, "PATCH", fmt.Sprintf("/authors/%s", id), payload)
	})

	s.AddTool(mcp.NewTool("update_series", opts("Rename a series or change its description. To move books between series or change sequence numbers use update_item",
		mcp.WithString("series_id", mcp.Required(), mcp.Description("Series ID")),
		mcp.WithString("name", mcp.Description("New name")),
		mcp.WithString("description", mcp.Description("Description")),
	)...), func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id, err := request.RequireString("series_id")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		args := request.GetArguments()
		payload := map[string]any{}
		for _, k := range []string{"name", "description"} {
			if hasArg(args, k) {
				payload[k] = request.GetString(k, "")
			}
		}
		if len(payload) == 0 {
			return mcp.NewToolResultError("nothing to update"), nil
		}
		return absCall(ctx, request, "PATCH", fmt.Sprintf("/series/%s", id), payload)
	})

	s.AddTool(mcp.NewTool("rename_narrator", opts("Rename a narrator across all books of a library (also merges into an existing narrator with the new name)",
		mcp.WithString("library_id", mcp.Required(), mcp.Description("Library ID")),
		mcp.WithString("name", mcp.Required(), mcp.Description("Current narrator name, exactly as stored")),
		mcp.WithString("new_name", mcp.Required(), mcp.Description("New narrator name")),
	)...), func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		lib, err := request.RequireString("library_id")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		name, err := request.RequireString("name")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		newName, err := request.RequireString("new_name")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return absCall(ctx, request, "PATCH", fmt.Sprintf("/libraries/%s/narrators/%s", lib, encodeFilterValue(name)), map[string]any{"name": newName})
	})

	// --- Genres / tags ---
	for _, kind := range []string{"genre", "tag"} {
		kind := kind
		plural := kind + "s"
		s.AddTool(mcp.NewTool("rename_"+kind, opts(fmt.Sprintf("Rename a %s on every item that has it (merges into an existing %s with the new name)", kind, kind),
			mcp.WithString(kind, mcp.Required(), mcp.Description(fmt.Sprintf("Current %s, exactly as stored", kind))),
			mcp.WithString("new_"+kind, mcp.Required(), mcp.Description(fmt.Sprintf("New %s", kind))),
		)...), func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			oldV, err := request.RequireString(kind)
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			newV, err := request.RequireString("new_" + kind)
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			return absCall(ctx, request, "POST", "/"+plural+"/rename", map[string]any{kind: oldV, "new" + strings.ToUpper(kind[:1]) + kind[1:]: newV})
		})
		s.AddTool(mcp.NewTool("delete_"+kind, opts(fmt.Sprintf("Remove a %s from every item that has it", kind),
			mcp.WithString(kind, mcp.Required(), mcp.Description(fmt.Sprintf("%s to remove, exactly as stored", kind))),
		)...), func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			v, err := request.RequireString(kind)
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			return absCall(ctx, request, "DELETE", "/"+plural+"/"+encodeFilterValue(v), nil)
		})
	}

	// --- Collections ---
	s.AddTool(mcp.NewTool("update_collection", opts("Rename a collection, change its description or set the order of its books",
		mcp.WithString("collection_id", mcp.Required(), mcp.Description("Collection ID")),
		mcp.WithString("name", mcp.Description("New name")),
		mcp.WithString("description", mcp.Description("Description")),
		mcp.WithArray("book_ids", mcp.WithStringItems(), mcp.Description("Library item IDs in the desired order (only reorders books already in the collection)")),
	)...), func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id, err := request.RequireString("collection_id")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		args := request.GetArguments()
		payload := map[string]any{}
		if hasArg(args, "name") {
			payload["name"] = request.GetString("name", "")
		}
		if hasArg(args, "description") {
			payload["description"] = request.GetString("description", "")
		}
		if ids := request.GetStringSlice("book_ids", nil); len(ids) > 0 {
			payload["books"] = ids
		}
		if len(payload) == 0 {
			return mcp.NewToolResultError("nothing to update"), nil
		}
		return absCall(ctx, request, "PATCH", fmt.Sprintf("/collections/%s", id), payload)
	})

	s.AddTool(mcp.NewTool("batch_add_to_collection", opts("Add several books to a collection, appended in the given order",
		mcp.WithString("collection_id", mcp.Required(), mcp.Description("Collection ID")),
		mcp.WithArray("book_ids", mcp.Required(), mcp.WithStringItems(), mcp.Description("Library item IDs")),
	)...), func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id, err := request.RequireString("collection_id")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		ids, err := request.RequireStringSlice("book_ids")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return absCall(ctx, request, "POST", fmt.Sprintf("/collections/%s/batch/add", id), map[string]any{"books": ids})
	})

	s.AddTool(mcp.NewTool("remove_from_collection", opts("Remove a book from a collection",
		mcp.WithString("collection_id", mcp.Required(), mcp.Description("Collection ID")),
		mcp.WithString("book_id", mcp.Required(), mcp.Description("Library item ID")),
	)...), func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id, err := request.RequireString("collection_id")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		book, err := request.RequireString("book_id")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return absCall(ctx, request, "DELETE", fmt.Sprintf("/collections/%s/book/%s", id, book), nil)
	})

	s.AddTool(mcp.NewTool("delete_collection", opts("Delete a collection (books themselves are not affected)",
		mcp.WithString("collection_id", mcp.Required(), mcp.Description("Collection ID")),
	)...), func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id, err := request.RequireString("collection_id")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return absCall(ctx, request, "DELETE", fmt.Sprintf("/collections/%s", id), nil)
	})
}
