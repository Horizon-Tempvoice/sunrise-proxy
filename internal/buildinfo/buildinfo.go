package buildinfo

import (
	"runtime"
	"runtime/debug"
)

// Set at build time with -ldflags "-X ...buildinfo.Version=... -X ...buildinfo.Commit=...".
var (
	Version = "dev"
	Commit  = ""
)

func GoVersion() string {
	return runtime.Version()
}

func CommitHash() string {
	if Commit != "" {
		return Commit
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	revision, modified := "", false
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.modified":
			modified = s.Value == "true"
		}
	}
	if revision == "" {
		return "unknown"
	}
	if len(revision) > 12 {
		revision = revision[:12]
	}
	if modified {
		revision += "-dirty"
	}
	return revision
}
