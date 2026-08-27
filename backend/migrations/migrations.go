// Package migrations holds the SQL schema migrations and exposes them as an
// embedded filesystem.
//
// The .sql files and this file live together because go:embed cannot reach
// outside its own package directory: internal/db, which runs the migrations,
// cannot embed ../migrations. Keeping the SQL at the top level of the module
// where it is easy to find, rather than buried inside the db package, costs
// exactly this one file.
package migrations

import "embed"

// FS holds every migration, named so that lexical order is application order.
//
//go:embed *.sql
var FS embed.FS
