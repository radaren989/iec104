// Package live drives a Station against a real IEC 60870-5-104 device.
//
// It holds no production code — the file exists so that `go build ./...` has a
// package to build here, and so this comment has somewhere to live.
//
// Why this package exists: every other test in this repository talks to code
// this repository also wrote. The outstation in internal/station answers the
// frames the station sends because the same person wrote both halves, which
// means a shared misreading of the standard passes every test we have. The
// packet captures under test/capture close part of that gap, but a capture is a
// recording: it cannot be asked a question, and it never disagrees.
//
// A live device can. This package is where a misreading finally has something
// to hit.
//
// It is skipped unless IEC104_SIM_ADDR names one, so `make check` is unaffected.
// That skip, rather than a build tag, is deliberate: a tagged file is not
// compiled, not vetted and not linted by the ordinary run, so it rots quietly
// and is discovered broken on the day someone needs it. This one compiles on
// every build and merely declines to run.
package live
