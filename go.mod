module github.com/talyvor/docs

go 1.25.0

// ⚠ THE VERSION THIS REPO SHIPS, AND ci.yaml MUST BE KEPT IN LOCKSTEP WITH IT BY HAND.
//
// Measured on this module, back-to-back so the advisory database could not move between runs:
// at go1.26.3 `govulncheck ./...` reported 10 CALLED vulnerabilities — the bucket govulncheck
// counts separately from "present in a module you require but your code doesn't appear to call".
// Nine were the standard library and this directive clears them: >= 1.26.6 covers GO-2026-6218,
// GO-2026-6090, GO-2026-6089, GO-2026-6088, GO-2026-5972, GO-2026-5039, GO-2026-5037 and
// GO-2026-5026; >= 1.26.5 covers GO-2026-5856 (crypto/tls ECH privacy leak).
//
// ⚠ THE DIRECTIVE ALONE DOES NOT REACH CI, AND talyvor-track LEARNED THAT THE EXPENSIVE WAY
// (its W6.34 first attempt merged nothing but an inert line until CI refused). actions/setup-go
// exports GOTOOLCHAIN=local, so every job runs exactly the version its `go-version:` pin
// installed and this directive is NOT auto-fetched there. It governs LOCAL builds and the Docker
// build; ci.yaml's pin governs CI. Both are 1.26.6 and internal/toolchainguard
// asserts the lockstep.
//
// ⚠ WHY THIS REPO NEVER NOTICED: it has no govulncheck job. talyvor-lens gates on one every
// build and reports zero called; docs and track did not and reported 10 and 11. Adding the gate
// here is W6.33 and needs the remaining advisory decided first, or it lands red on main.
toolchain go1.26.6

require (
	github.com/go-chi/chi/v5 v5.3.0
	github.com/go-pdf/fpdf v0.9.0
	github.com/gorilla/websocket v1.5.3
	github.com/jackc/pgx/v5 v5.9.2
	github.com/pashagolub/pgxmock/v4 v4.9.0
	github.com/prometheus/client_golang v1.23.2
	github.com/yuin/goldmark v1.8.2
	golang.org/x/crypto v0.52.0
	golang.org/x/net v0.55.0
	golang.org/x/time v0.15.0
)

require (
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/prometheus/client_model v0.6.2 // indirect
	github.com/prometheus/common v0.66.1 // indirect
	github.com/prometheus/procfs v0.16.1 // indirect
	github.com/rogpeppe/go-internal v1.14.1 // indirect
	go.yaml.in/yaml/v2 v2.4.2 // indirect
	golang.org/x/sync v0.21.0 // indirect
	golang.org/x/sys v0.45.0 // indirect
	golang.org/x/text v0.39.0 // indirect
	google.golang.org/protobuf v1.36.8 // indirect
)
