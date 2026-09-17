// Package version contains the release metadata shared by every ONYX daemon.
//
// Version and Commit are variables (rather than constants) so release builds
// can inject their exact tag and source revision with -ldflags. Development
// builds retain useful, explicit defaults.
package version

const (
	// APIVersion is the stable REST namespace. Breaking API changes require a
	// new major namespace; additive changes remain v1.
	APIVersion = "v1"
	// Codename identifies the current roadmap milestone.
	Codename = "Obsidian"
)

var (
	Version = "0.3.0-dev"
	Commit  = "unknown"
)

// Info is the complete version response shared by command-line and HTTP
// surfaces.
type Info struct {
	Version    string `json:"version"`
	APIVersion string `json:"api_version"`
	Codename   string `json:"codename"`
	Commit     string `json:"commit,omitempty"`
}

// Current returns a snapshot of the running build's release metadata.
func Current() Info {
	return Info{
		Version:    Version,
		APIVersion: APIVersion,
		Codename:   Codename,
		Commit:     Commit,
	}
}
