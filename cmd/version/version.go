package version

import (
	"runtime/debug"
	"time"
)

type BuildInfo struct {
	Version  string
	GitRev   string
	GitDirty bool
}

// Placeholder values stamped in below; overridden by the generated file
// (see //go:generate) and, failing that, by ReadBuildInfo in Get.
const (
	placeholderVersion = "YYMMDD-local"
	placeholderRev     = "0000000000000000000000000000000000000000"
)

//go:generate bash -c "../../scripts/gen-version-file.sh > /dev/null"
var build_info = BuildInfo{
	Version:  placeholderVersion,
	GitRev:   placeholderRev,
	GitDirty: false,
}

// Get returns the build metadata. When the values are still the compiled-in
// placeholders (i.e. `go generate` did not stamp version.g.go), it falls back
// to the VCS information Go records in the binary via runtime/debug — a normal
// `go build` stamps vcs.revision/modified/time unless -buildvcs=false.
func Get() BuildInfo {
	b := build_info
	if b.Version != placeholderVersion || b.GitRev != placeholderRev {
		// Stamped by the generated file; trust it as-is.
		return b
	}

	info, ok := debug.ReadBuildInfo()
	if !ok {
		return b
	}
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			if s.Value != "" {
				b.GitRev = s.Value
			}
		case "vcs.modified":
			b.GitDirty = s.Value == "true"
		case "vcs.time":
			if t, err := time.Parse(time.RFC3339, s.Value); err == nil {
				b.Version = t.UTC().Format("060102")
			}
		}
	}
	return b
}
