// SPDX-License-Identifier: Apache-2.0

package notionx

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// The response shapes of the published operations, restricted to the fields the connector reads. A
// field Notion adds is ignored. Fields that name a person (`created_by`, `last_edited_by`, people
// properties) are deliberately absent: what is never decoded is never carried.

// Me is the integration's own bot user.
type Me struct {
	ID        string
	Workspace string
}

// UsersMe reads the bot user, whose workspace name the configured label is checked against.
func (c *Client) UsersMe(ctx context.Context) (Me, error) {
	raw, err := c.Do(ctx, OpUsersMe, nil, nil, nil)
	if err != nil {
		return Me{}, err
	}
	var body struct {
		ID   string `json:"id"`
		Type string `json:"type"`
		Bot  struct {
			WorkspaceName string `json:"workspace_name"`
		} `json:"bot"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return Me{}, fmt.Errorf("notionx: users/me: %w", err)
	}
	if body.Type != "bot" {
		return Me{}, fmt.Errorf("notionx: users/me answered a %q user; an integration secret is a bot's", body.Type)
	}
	return Me{ID: body.ID, Workspace: body.Bot.WorkspaceName}, nil
}

// Database is a database container and its data sources.
type Database struct {
	ID          string
	Title       string
	DataSources []DataSourceRef
	InTrash     bool
}

// DataSourceRef is one data source of a database.
type DataSourceRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Database reads one database.
func (c *Client) Database(ctx context.Context, id string) (Database, error) {
	raw, err := c.Do(ctx, OpDatabase, []string{id}, nil, nil)
	if err != nil {
		return Database{}, err
	}
	var body struct {
		ID          string          `json:"id"`
		Title       []RichText      `json:"title"`
		DataSources []DataSourceRef `json:"data_sources"`
		InTrash     bool            `json:"in_trash"`
		Archived    bool            `json:"archived"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return Database{}, fmt.Errorf("notionx: databases/%s: %w", id, err)
	}
	return Database{ID: body.ID, Title: PlainText(body.Title), DataSources: body.DataSources,
		InTrash: body.InTrash || body.Archived}, nil
}

// Page is one page of a data source.
type Page struct {
	ID             string              `json:"id"`
	URL            string              `json:"url"`
	CreatedTime    time.Time           `json:"created_time"`
	LastEditedTime time.Time           `json:"last_edited_time"`
	InTrash        bool                `json:"in_trash"`
	Archived       bool                `json:"archived"`
	Parent         Parent              `json:"parent"`
	Properties     map[string]Property `json:"properties"`
}

// Gone reports a page in the trash or archived.
func (p Page) Gone() bool { return p.InTrash || p.Archived }

// Parent is where a page lives.
type Parent struct {
	Type         string `json:"type"`
	DataSourceID string `json:"data_source_id"`
	DatabaseID   string `json:"database_id"`
}

// Property is one property value of a page. Only the types a binding may name are decoded; a people
// property, or any other, keeps its id and type and nothing else.
type Property struct {
	ID          string        `json:"id"`
	Type        string        `json:"type"`
	Title       []RichText    `json:"title,omitempty"`
	RichText    []RichText    `json:"rich_text,omitempty"`
	Select      *Option       `json:"select,omitempty"`
	Status      *Option       `json:"status,omitempty"`
	MultiSelect []Option      `json:"multi_select,omitempty"`
	Relation    []RelationRef `json:"relation,omitempty"`
	HasMore     bool          `json:"has_more,omitempty"`
	Date        *DateValue    `json:"date,omitempty"`
}

// Option is a select, status or multi-select option.
type Option struct {
	Name string `json:"name"`
}

// RelationRef is one related page.
type RelationRef struct {
	ID string `json:"id"`
}

// DateValue is a date property's value.
type DateValue struct {
	Start string `json:"start"`
}

// RichText is one span of rich text.
type RichText struct {
	Type      string `json:"type"`
	PlainText string `json:"plain_text"`
	Mention   *struct {
		Type string `json:"type"`
	} `json:"mention,omitempty"`
}

// PlainText reduces rich text to plain text, dropping every user mention: a person named in a
// summary is not part of what retrieval ranks (FR-023, research R8).
func PlainText(spans []RichText) string {
	var b strings.Builder
	for _, s := range spans {
		if s.Type == "mention" && s.Mention != nil && s.Mention.Type == "user" {
			continue
		}
		b.WriteString(s.PlainText)
	}
	return strings.TrimSpace(b.String())
}

// Text is the property's value as plain text, for the property types a summary may name: title, rich
// text, select and status. Any other type has no text.
func (p Property) Text() string {
	switch p.Type {
	case "title":
		return PlainText(p.Title)
	case "rich_text":
		return PlainText(p.RichText)
	case "select":
		if p.Select != nil {
			return p.Select.Name
		}
	case "status":
		if p.Status != nil {
			return p.Status.Name
		}
	}
	return ""
}

// Values are the property's values for an entity map: option names for a select, status or
// multi-select, related page ids for a relation.
func (p Property) Values() []string {
	var out []string
	switch p.Type {
	case "select":
		if p.Select != nil && p.Select.Name != "" {
			out = append(out, p.Select.Name)
		}
	case "status":
		if p.Status != nil && p.Status.Name != "" {
			out = append(out, p.Status.Name)
		}
	case "multi_select":
		for _, o := range p.MultiSelect {
			if o.Name != "" {
				out = append(out, o.Name)
			}
		}
	case "relation":
		for _, r := range p.Relation {
			if r.ID != "" {
				out = append(out, r.ID)
			}
		}
	}
	return out
}

// Truncated reports a value cut at Notion's 25-reference limit in a query result (research R7).
func (p Property) Truncated() bool {
	const limit = 25
	switch p.Type {
	case "relation":
		return p.HasMore || len(p.Relation) >= limit
	case "title":
		return len(p.Title) >= limit
	case "rich_text":
		return len(p.RichText) >= limit
	}
	return false
}

// QueryPage is one page of a data-source query.
type QueryPage struct {
	Pages      []Page
	HasMore    bool
	NextCursor string
}

// QueryFilter is the part of a query's body the connector sets: an optional lower bound on
// `last_edited_time`, always sorted ascending on it.
type QueryFilter struct {
	EditedOnOrAfter time.Time
}

// QueryDataSource reads one page of a data source's pages.
func (c *Client) QueryDataSource(ctx context.Context, id string, filter QueryFilter, cursor string) (QueryPage, error) {
	body := map[string]any{
		"page_size": 100,
		"sorts":     []map[string]string{{"timestamp": "last_edited_time", "direction": "ascending"}},
	}
	if !filter.EditedOnOrAfter.IsZero() {
		body["filter"] = map[string]any{
			"timestamp":        "last_edited_time",
			"last_edited_time": map[string]string{"on_or_after": filter.EditedOnOrAfter.UTC().Format(time.RFC3339)},
		}
	}
	if cursor != "" {
		body["start_cursor"] = cursor
	}
	raw, err := c.Do(ctx, OpQueryDataSource, []string{id}, nil, body)
	if err != nil {
		return QueryPage{}, err
	}
	var out struct {
		Results    []Page `json:"results"`
		HasMore    bool   `json:"has_more"`
		NextCursor string `json:"next_cursor"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return QueryPage{}, fmt.Errorf("notionx: data_sources/%s/query: %w", id, err)
	}
	return QueryPage{Pages: out.Results, HasMore: out.HasMore, NextCursor: out.NextCursor}, nil
}

// Page reads one page by id.
func (c *Client) Page(ctx context.Context, id string) (Page, error) {
	raw, err := c.Do(ctx, OpPage, []string{id}, nil, nil)
	if err != nil {
		return Page{}, err
	}
	var page Page
	if err := json.Unmarshal(raw, &page); err != nil {
		return Page{}, fmt.Errorf("notionx: pages/%s: %w", id, err)
	}
	return page, nil
}

// PropertyItems reads every item of one page property, paginated, until done or until stop says the
// caller has enough. The items come back as a property of the same type, so Text and Values read it.
func (c *Client) PropertyItems(ctx context.Context, pageID, propertyID string, stop func(Property) bool) (Property, error) {
	var out Property
	cursor := ""
	for {
		query := url.Values{}
		if cursor != "" {
			query.Set("start_cursor", cursor)
		}
		raw, err := c.Do(ctx, OpPageProperty, []string{pageID, propertyID}, query, nil)
		if err != nil {
			return out, err
		}
		var probe struct {
			Object string `json:"object"`
		}
		if err := json.Unmarshal(raw, &probe); err != nil {
			return out, fmt.Errorf("notionx: pages/%s/properties/%s: %w", pageID, propertyID, err)
		}
		if probe.Object != "list" {
			// A property that is not paginated comes back as the property itself.
			var single Property
			if err := json.Unmarshal(raw, &single); err != nil {
				return out, fmt.Errorf("notionx: pages/%s/properties/%s: %w", pageID, propertyID, err)
			}
			return single, nil
		}
		var body struct {
			Results []struct {
				Type     string       `json:"type"`
				Title    *RichText    `json:"title"`
				RichText *RichText    `json:"rich_text"`
				Relation *RelationRef `json:"relation"`
			} `json:"results"`
			HasMore    bool   `json:"has_more"`
			NextCursor string `json:"next_cursor"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			return out, fmt.Errorf("notionx: pages/%s/properties/%s: %w", pageID, propertyID, err)
		}
		out.ID = propertyID
		for _, item := range body.Results {
			out.Type = item.Type
			switch {
			case item.Title != nil:
				out.Title = append(out.Title, *item.Title)
			case item.RichText != nil:
				out.RichText = append(out.RichText, *item.RichText)
			case item.Relation != nil:
				out.Relation = append(out.Relation, *item.Relation)
			}
		}
		if !body.HasMore || body.NextCursor == "" || (stop != nil && stop(out)) {
			return out, nil
		}
		cursor = body.NextCursor
	}
}

// Block is one block of a page body (008 research R6), reduced to what a section needs: its type,
// whether it has children, and its plain text. A block's `created_by` and `last_edited_by` are never
// decoded, and neither is a file, an image or an embed's URL.
type Block struct {
	ID          string
	Type        string
	HasChildren bool
	// RichText is the block's text, for the types that carry one under `rich_text` (paragraphs,
	// headings, list items, quotes, callouts, toggles, to-dos, code).
	RichText []RichText
	// Cells are a table row's cells.
	Cells [][]RichText
	// SyncedFrom marks a synced block whose original lives on another page.
	SyncedFrom bool
}

// Text is the block's text, user mentions dropped; a table row's cells are joined with " | ".
func (b Block) Text() string {
	if b.Type == "table_row" {
		cells := make([]string, 0, len(b.Cells))
		for _, cell := range b.Cells {
			cells = append(cells, PlainText(cell))
		}
		return strings.TrimSpace(strings.Join(cells, " | "))
	}
	return PlainText(b.RichText)
}

// UnmarshalJSON reads the block's type-named object for its text.
func (b *Block) UnmarshalJSON(raw []byte) error {
	var head struct {
		ID          string `json:"id"`
		Type        string `json:"type"`
		HasChildren bool   `json:"has_children"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return err
	}
	*b = Block{ID: head.ID, Type: head.Type, HasChildren: head.HasChildren}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	body, ok := fields[head.Type]
	if !ok || len(body) == 0 || string(body) == "null" {
		return nil
	}
	var content struct {
		RichText   []RichText      `json:"rich_text"`
		Cells      [][]RichText    `json:"cells"`
		SyncedFrom json.RawMessage `json:"synced_from"`
	}
	if err := json.Unmarshal(body, &content); err != nil {
		return fmt.Errorf("notionx: block %s (%s): %w", head.ID, head.Type, err)
	}
	b.RichText, b.Cells = content.RichText, content.Cells
	b.SyncedFrom = len(content.SyncedFrom) > 0 && string(content.SyncedFrom) != "null"
	return nil
}

// BlockPage is one page of a block's children.
type BlockPage struct {
	Blocks     []Block
	HasMore    bool
	NextCursor string
}

// BlockChildren reads one page, of at most 100, of a block's children. A page id is a block id.
func (c *Client) BlockChildren(ctx context.Context, id, cursor string) (BlockPage, error) {
	query := url.Values{}
	query.Set("page_size", "100")
	if cursor != "" {
		query.Set("start_cursor", cursor)
	}
	raw, err := c.Do(ctx, OpBlockChildren, []string{id}, query, nil)
	if err != nil {
		return BlockPage{}, err
	}
	var out struct {
		Results    []Block `json:"results"`
		HasMore    bool    `json:"has_more"`
		NextCursor string  `json:"next_cursor"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return BlockPage{}, fmt.Errorf("notionx: blocks/%s/children: %w", id, err)
	}
	return BlockPage{Blocks: out.Results, HasMore: out.HasMore, NextCursor: out.NextCursor}, nil
}
