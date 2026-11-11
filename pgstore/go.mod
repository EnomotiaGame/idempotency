module github.com/EnomotiaGame/idempotency/pgstore

go 1.23.0

require (
	github.com/EnomotiaGame/idempotency v0.1.0
	github.com/jackc/pgx/v5 v5.6.0
)

require (
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20221227161230-091c0ba34f0a // indirect
	github.com/jackc/puddle/v2 v2.2.1 // indirect
	golang.org/x/crypto v0.36.0 // indirect
	golang.org/x/sync v0.12.0 // indirect
	golang.org/x/text v0.23.0 // indirect
)

// The parent module, from this repository. A replace in a dependency is
// ignored by the modules that use it, so users get the tagged release.
replace github.com/EnomotiaGame/idempotency => ../
