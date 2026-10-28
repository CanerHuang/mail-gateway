// Package version exposes build-time metadata. All three vars are populated
// by `-ldflags -X` in build.sh; the defaults below are what `go run` / IDE
// builds will show.
package version

var (
	Version = "dev"
	Commit  = "unknown"
	Date    = "unknown"
)

// Info renders the short form used in startup log: "<version>-<commit>-<date>".
func Info() string {
	return Version + "-" + Commit + "-" + Date
}

// Detailed renders a multi-line human-readable form for CLI `-version` output.
func Detailed() string {
	return "version: " + Version + "\ncommit:  " + Commit + "\ndate:    " + Date
}
