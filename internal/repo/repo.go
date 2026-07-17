// Package repo provides thin, testable repositories over the GORM data model.
// Every repository is constructed with a *gorm.DB and exposes a WithTx method so
// callers can compose operations inside a transaction. Repositories contain no
// business logic beyond query shaping and the small amount of atomicity the data
// model requires (e.g. queue claiming).
package repo

import "errors"

// ErrNotFound is returned by Get-style lookups when no matching, non-deleted row
// exists. Find-style lookups (FindByX) instead return (nil, nil) for "no match".
var ErrNotFound = errors.New("repo: record not found")

// Page describes a pagination window. A Limit of 0 means "no limit".
type Page struct {
	Limit  int
	Offset int
}

// apply returns the limit/offset to use, normalizing negatives to zero.
func (p Page) apply() (limit, offset int) {
	limit = p.Limit
	if limit < 0 {
		limit = 0
	}
	offset = p.Offset
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}
