package websim

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

// CommentScope binds comment calls to a project, because these endpoints
// require a Referer pointing at the project's public page. The slug is not
// interchangeable with the project ID.
type CommentScope struct {
	ProjectID string
	Slug      string
}

func (s CommentScope) validate() error {
	if s.ProjectID == "" {
		return fmt.Errorf("comment scope: project id is required")
	}
	if s.Slug == "" {
		return fmt.Errorf("comment scope: project slug is required for the Referer header")
	}
	return nil
}

// ListComments returns top-level comments, newest-sorted by created_at.
func (c *Client) ListComments(ctx context.Context, scope CommentScope, limit int) ([]Comment, error) {
	if err := scope.validate(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 20
	}

	q := url.Values{}
	q.Set("first", strconv.Itoa(limit))
	q.Set("sort_by", "created_at")
	q.Set("only_video", "false")

	body, err := c.do(ctx, request{
		op:      "list comments",
		method:  http.MethodGet,
		url:     c.apiURL("/projects/" + scope.ProjectID + "/comments?" + q.Encode()),
		referer: commentReferer(scope.Slug),
	})
	if err != nil {
		return nil, err
	}
	return normComments(body)
}

// ListReplies returns replies to a comment.
func (c *Client) ListReplies(ctx context.Context, scope CommentScope, commentID string, limit int) ([]Comment, error) {
	if err := scope.validate(); err != nil {
		return nil, err
	}
	if commentID == "" {
		return nil, fmt.Errorf("list replies: comment id is required")
	}
	if limit <= 0 {
		limit = 20
	}

	q := url.Values{}
	q.Set("last", strconv.Itoa(limit))

	body, err := c.do(ctx, request{
		op:      "list replies",
		method:  http.MethodGet,
		url:     c.apiURL("/projects/" + scope.ProjectID + "/comments/" + url.PathEscape(commentID) + "/replies?" + q.Encode()),
		referer: commentReferer(scope.Slug),
	})
	if err != nil {
		return nil, err
	}
	return normComments(body)
}

// PostComment posts a top-level comment. Pass a non-empty parentCommentID to
// post a reply instead; there is no separate reply endpoint.
//
// The playbook warns against posting a duplicate after an ambiguous timeout.
// The transport layer will not retry a POST whose response was lost, so a
// caller that sees a network error must re-list before trying again.
func (c *Client) PostComment(ctx context.Context, scope CommentScope, content, parentCommentID string) ([]byte, error) {
	if err := scope.validate(); err != nil {
		return nil, err
	}
	if content == "" {
		return nil, fmt.Errorf("post comment: content is empty")
	}

	payload := map[string]any{"content": content}
	op := "post comment"
	if parentCommentID != "" {
		payload["parent_comment_id"] = parentCommentID
		op = "post reply"
	}

	encoded, err := jsonBytes(payload)
	if err != nil {
		return nil, err
	}
	return c.do(ctx, request{
		op:          op,
		method:      http.MethodPost,
		url:         c.apiURL("/projects/" + scope.ProjectID + "/comments"),
		contentType: "application/json",
		newBody:     bytesReaderFn(encoded),
		referer:     commentReferer(scope.Slug),
	})
}

// DeleteComment removes a comment.
func (c *Client) DeleteComment(ctx context.Context, scope CommentScope, commentID string) error {
	if err := scope.validate(); err != nil {
		return err
	}
	if commentID == "" {
		return fmt.Errorf("delete comment: comment id is required")
	}

	_, err := c.do(ctx, request{
		op:      "delete comment",
		method:  http.MethodDelete,
		url:     c.apiURL("/projects/" + scope.ProjectID + "/comments/" + url.PathEscape(commentID)),
		referer: commentReferer(scope.Slug),
	})
	return err
}
