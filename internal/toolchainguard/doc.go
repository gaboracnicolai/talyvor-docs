// Package toolchainguard holds no product code. It exists so the Go version this repository
// SHIPS and the Go version its CI RUNS cannot drift apart silently.
//
// # Why this needs an instrument rather than a comment
//
// There are two version numbers and nothing but a person keeps them equal:
//
//	go.mod `toolchain go1.26.6`      LOCAL builds and the Docker build
//	ci.yaml `go-version: "1.26.6"`   every CI job
//
// ⚠ THE DIRECTIVE DOES NOT REACH CI. actions/setup-go exports GOTOOLCHAIN=local, so each job runs
// exactly the version its pin installed and go.mod's floor is never auto-fetched there. That is not
// a reading of the documentation — talyvor-track proved it the expensive way in W6.34, merging a
// toolchain directive whose commit message claimed an 11 → 2 improvement, while CI still built with
// go1.25.14 and golangci-lint refused to start. The pin was a comment. This package is what stops
// the same thing being true here.
//
// # What the floor is for
//
// Measured on this module back-to-back, seconds apart so the advisory database could not move:
//
//	go1.26.3   govulncheck: "your code is affected by 10 vulnerabilities"
//	go1.26.6   govulncheck: "your code is affected by  1 vulnerability"
//
// CALLED, not the separate "present in a module you require but your code doesn't appear to call"
// bucket. Nine of the ten were the standard library.
//
// ⚠ THE ONE THAT REMAINS IS NOT A TOOLCHAIN PROBLEM AND IS DELIBERATELY NOT FIXED HERE:
// GO-2026-5970, an infinite loop on invalid input in golang.org/x/text, found at v0.37.0 and fixed
// at v0.39.0. talyvor-track carries the same advisory at v0.18.0. A dependency bump is a decision
// with the whole module graph behind it, and W6.35's claim said in advance that any residue would
// be named and left rather than bundled in — so it is named, in W6.33, and left.
//
// ⚠ AND THE REASON NOBODY NOTICED FOR EITHER REPO: neither has a govulncheck job. talyvor-lens
// gates on one every build and reports zero called. Adding the gate here needs the x/text bump
// decided first, or it lands red on main — which is exactly the ordering W6.33 exists to hold open.
package toolchainguard
