// Package ent contains the database schema (in ./schema) and the type-safe
// client generated from it. Everything in this directory except generate.go
// and schema/ is generated: never edit it by hand, run `make generate`.
package ent

//go:generate go tool ent generate --feature sql/execquery ./schema
