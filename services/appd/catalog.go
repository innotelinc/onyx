package main

import (
	"fmt"
	"regexp"
	"strings"

	onyxv1 "github.com/innotelinc/onyx/proto/gen/go/onyx/v1"
)

// The curated catalog (docs/design/09). Manifests are compose files; they are
// the app's definition, kept in code until the signed store service lands
// (docs/design/11 §6.4). `{{key}}` placeholders are filled from the install
// request's config — deliberately not `${key}`, which compose interpolates
// from the environment, so an app's own variables and Onyx's settings can never
// be confused for one another.
type catalogApp struct {
	ID          string
	Name        string
	Version     string
	Description string
	Manifest    string
	// Defaults are the install-time settings offered to the operator.
	Defaults map[string]string
	// PathKeys are the config keys whose value is a host path. They are
	// substituted into bind mounts, so their values are confined to the storage
	// root (validateConfigPaths) rather than trusted from the install request.
	PathKeys []string
}

// appIDRe keeps an app id safe as a compose project name and directory name.
var appIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

func catalog() map[string]catalogApp {
	apps := []catalogApp{
		{
			ID:          "jellyfin",
			Name:        "Jellyfin",
			Version:     "10.9",
			Description: "Media server for movies, shows and music",
			Defaults:    map[string]string{"http_port": "8096", "media_path": "/mnt/onyx"},
			PathKeys:    []string{"media_path"},
			Manifest: `# Onyx app manifest: Jellyfin (docs/design/09).
# Sandbox posture (docs/design/09 §6): no new privileges, every capability
# dropped, resource limits, and storage limited to the pool.
services:
  jellyfin:
    image: jellyfin/jellyfin:10.9
    container_name: onyx-jellyfin
    restart: unless-stopped
    ports:
      - "{{http_port}}:8096"
    volumes:
      - jellyfin-config:/config
      - jellyfin-cache:/cache
      - "{{media_path}}:/media:ro"
    security_opt:
      - no-new-privileges:true
    cap_drop:
      - ALL
    pids_limit: 512
    mem_limit: 2g
volumes:
  jellyfin-config:
  jellyfin-cache:
`,
		},
		{
			ID:          "nextcloud",
			Name:        "Nextcloud",
			Version:     "29",
			Description: "File sync and share with a web UI",
			Defaults:    map[string]string{"http_port": "8080", "data_path": "/mnt/onyx", "db_password": "onyx"},
			PathKeys:    []string{"data_path"},
			Manifest: `# Onyx app manifest: Nextcloud (docs/design/09).
# Two services: the app and its database. The database is private to the
# project's network, so only the app publishes a port. Both carry the platform
# sandbox posture (docs/design/09 §6).
services:
  app:
    image: nextcloud:29-apache
    container_name: onyx-nextcloud
    restart: unless-stopped
    depends_on:
      - db
    ports:
      - "{{http_port}}:80"
    environment:
      NEXTCLOUD_TRUSTED_DOMAINS: "{{host}}"
      MYSQL_HOST: db
      MYSQL_DATABASE: nextcloud
      MYSQL_USER: nextcloud
      MYSQL_PASSWORD: "{{db_password}}"
    volumes:
      - nextcloud-data:/var/www/html
      - "{{data_path}}/nextcloud:/data"
    security_opt:
      - no-new-privileges:true
    cap_drop:
      - ALL
    pids_limit: 512
    mem_limit: 2g
  db:
    image: mariadb:11
    container_name: onyx-nextcloud-db
    restart: unless-stopped
    environment:
      MARIADB_DATABASE: nextcloud
      MARIADB_USER: nextcloud
      MARIADB_PASSWORD: "{{db_password}}"
      MARIADB_ROOT_PASSWORD: "{{db_password}}"
    volumes:
      - nextcloud-db:/var/lib/mysql
    security_opt:
      - no-new-privileges:true
    cap_drop:
      - ALL
    pids_limit: 512
    mem_limit: 2g
volumes:
  nextcloud-data:
  nextcloud-db:
`,
		},
		{
			ID:          "photoprism",
			Name:        "PhotoPrism",
			Version:     "240717",
			Description: "Photo manager with search and face recognition",
			Defaults:    map[string]string{"http_port": "2342", "originals_path": "/mnt/onyx/photos"},
			PathKeys:    []string{"originals_path"},
			Manifest: `# Onyx app manifest: PhotoPrism (docs/design/09).
# Sandbox posture (docs/design/09 §6): no new privileges, every capability
# dropped, resource limits, and storage limited to the pool.
services:
  photoprism:
    image: photoprism/photoprism:240717
    container_name: onyx-photoprism
    restart: unless-stopped
    ports:
      - "{{http_port}}:2342"
    environment:
      PHOTOPRISM_ADMIN_PASSWORD: "{{admin_password}}"
      PHOTOPRISM_SITE_URL: "http://{{host}}:{{http_port}}/"
      PHOTOPRISM_ORIGINALS_PATH: /photoprism/originals
    volumes:
      - "{{originals_path}}:/photoprism/originals"
      - photoprism-storage:/photoprism/storage
    security_opt:
      - no-new-privileges:true
    cap_drop:
      - ALL
    pids_limit: 512
    mem_limit: 2g
volumes:
  photoprism-storage:
`,
		},
	}

	out := make(map[string]catalogApp, len(apps))
	for _, a := range apps {
		out[a.ID] = a
	}
	return out
}

// validateCatalog rejects a catalog entry that could not be installed: a bad
// id (it names a compose project and a directory), a manifest with nothing in
// it, or a manifest that breaks the app sandbox (docs/design/09 §6). Called at
// startup so a broken entry fails loudly instead of becoming a confusing install
// error — or a privileged container — later.
func validateCatalog(apps map[string]catalogApp) error {
	for id, app := range apps {
		if err := validateAppID(id); err != nil {
			return err
		}
		if strings.TrimSpace(app.Manifest) == "" {
			return fmt.Errorf("app %q has an empty manifest", id)
		}
		if err := validateManifestHardening(id, app.Manifest); err != nil {
			return err
		}
	}
	return nil
}

// catalogConfig merges the operator's settings over the catalog defaults.
func catalogConfig(app catalogApp, requested map[string]string) map[string]string {
	cfg := make(map[string]string, len(app.Defaults)+len(requested))
	for k, v := range app.Defaults {
		cfg[k] = v
	}
	for k, v := range requested {
		if v != "" {
			cfg[k] = v
		}
	}
	return cfg
}

// catalogAppToProto renders one catalog entry, with the installed overlay
// applied when the app is installed.
func catalogAppToProto(app catalogApp, installed *installedApp) *onyxv1.App {
	out := &onyxv1.App{
		Id:          app.ID,
		Name:        app.Name,
		Version:     app.Version,
		Description: app.Description,
		Manifest:    app.Manifest,
		Status:      "not_installed",
		Config:      catalogConfig(app, nil),
	}
	if installed == nil {
		return out
	}
	out.Status = "installed"
	out.InstalledAt = installed.InstalledAt
	if installed.Version != "" {
		out.Version = installed.Version
	}
	out.Config = catalogConfig(app, installed.Config)
	return out
}
