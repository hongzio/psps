package client

import (
	"context"
	"fmt"
	"net/url"
	"strconv"

	"github.com/hongzio/psps/internal/api"
	"github.com/hongzio/psps/internal/store"
)

// Kanban is isolation iso's boards and their cards (every board when iso
// is ""), narrowed to board and status when given.
func (c *Client) Kanban(ctx context.Context, iso, board, status string) (api.Kanban, error) {
	q := url.Values{}
	for k, v := range map[string]string{"iso": iso, "board": board, "status": status} {
		if v != "" {
			q.Set(k, v)
		}
	}
	var v api.Kanban
	return v, c.do(ctx, "GET", "/v1/kanban?"+q.Encode(), nil, &v)
}

// KanbanLog is the history of isolation iso's boards (every board when
// iso is ""), or of one board, newest first.
func (c *Client) KanbanLog(ctx context.Context, iso, board string, limit int) ([]api.KanbanEvent, error) {
	q := url.Values{}
	if iso != "" {
		q.Set("iso", iso)
	}
	if board != "" {
		q.Set("board", board)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	var v []api.KanbanEvent
	return v, c.do(ctx, "GET", "/v1/kanban/log?"+q.Encode(), nil, &v)
}

func cardPath(id int64) string { return fmt.Sprintf("/v1/kanban/cards/%d", id) }

func (c *Client) KanbanCard(ctx context.Context, id int64) (api.KanbanCardDetail, error) {
	var v api.KanbanCardDetail
	return v, c.do(ctx, "GET", cardPath(id), nil, &v)
}

func (c *Client) AddKanbanCard(ctx context.Context, req api.NewCard) (api.KanbanCard, error) {
	var v api.KanbanCard
	return v, c.do(ctx, "POST", "/v1/kanban/cards", req, &v)
}

func (c *Client) UpdateKanbanCard(ctx context.Context, id int64, p store.CardPatch) (api.KanbanCard, error) {
	var v api.KanbanCard
	return v, c.do(ctx, "PATCH", cardPath(id), p, &v)
}

func (c *Client) DeleteKanbanCard(ctx context.Context, id int64) (api.KanbanCard, error) {
	var v api.KanbanCard
	return v, c.do(ctx, "DELETE", cardPath(id), nil, &v)
}

func (c *Client) RestoreKanbanCard(ctx context.Context, id int64) (api.KanbanCard, error) {
	var v api.KanbanCard
	return v, c.do(ctx, "POST", cardPath(id)+"/restore", nil, &v)
}

func (c *Client) AddKanbanComment(ctx context.Context, card int64, text string) (store.KanbanComment, error) {
	var v store.KanbanComment
	return v, c.do(ctx, "POST", cardPath(card)+"/comments", api.KanbanText{Text: text}, &v)
}

// ChangeKanbanComment edits a comment of the operator's, or deletes it
// (text nil).
func (c *Client) ChangeKanbanComment(ctx context.Context, id int64, text *string) (store.KanbanComment, error) {
	var v store.KanbanComment
	path := fmt.Sprintf("/v1/kanban/comments/%d", id)
	if text == nil {
		return v, c.do(ctx, "DELETE", path, nil, &v)
	}
	return v, c.do(ctx, "PATCH", path, api.KanbanText{Text: *text}, &v)
}
