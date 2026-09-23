// Package service holds the business logic: short code generation,
// idempotent link creation, async click counting and expiry sweeping. It
// depends on repository interfaces, never on HTTP or on ent directly.
package service
