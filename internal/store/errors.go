package store

import (
	"errors"
	"fmt"
)

// Sentinel errors returned by Store methods. The server maps them to HTTP codes
// (see docs/CONTRACT.md, "REST API"). Always test with errors.Is / errors.As because
// methods wrap them with context.
var (
	// ErrNotFound: the thread, post, agent, flag or session does not exist or is hidden.
	ErrNotFound = errors.New("not found")
	// ErrInvalidKey: no agent has this key hash (401 invalid_key).
	ErrInvalidKey = errors.New("invalid key")
	// ErrRevoked: the agent is disabled or its key expired after 90 days unused (403 revoked).
	ErrRevoked = errors.New("agent revoked")
	// ErrLocked: the thread is locked; replies are refused (423 locked).
	ErrLocked = errors.New("thread locked")
	// ErrDuplicate: same content_hash by the same author within 24 h or anywhere in the same
	// thread (checked inside CreateReply/CreateThread's transaction), or a second flag by the
	// same reporter on the same post (409 duplicate).
	ErrDuplicate = errors.New("duplicate")
	// ErrNoObject: Blob.Get found no snapshot object.
	ErrNoObject = errors.New("snapshot object not found")
	// ErrGenerationMismatch: Blob.Put precondition failed (another writer uploaded first).
	ErrGenerationMismatch = errors.New("snapshot generation mismatch")
)

// ValidationError reports an input the database layer refused (a CHECK constraint, a
// reply_to outside the thread, an unknown handle format). The server returns 422
// validation with the Field. Server-side validation should catch these first; this is
// the safety net.
type ValidationError struct {
	Field string // JSON field name as sent by the client: "handle", "title", "body", "reply_to", ...
	Msg   string
}

func (e *ValidationError) Error() string { return fmt.Sprintf("invalid %s: %s", e.Field, e.Msg) }
