// Package capture replays a real IEC 60870-5-104 packet capture through the
// protocol packages.
//
// It holds no production code — the file exists so that `go build ./...` has a
// package to build here, and so this comment has somewhere to live.
//
// Why this package exists: every other golden vector in this repository was
// written by hand from the standard. Those prove the implementation agrees with
// the specification, which is necessary and not sufficient. Interoperability
// bugs live exactly where a device and the specification disagree, and the only
// way to find those is to replay what a device actually sent.
package capture
