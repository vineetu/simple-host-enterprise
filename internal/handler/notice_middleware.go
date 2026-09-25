package handler

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

const (
	MinimumSupportedSkillVersion = "0.11.0"
	skillUpdateURL               = "/install.html"

	headerSkillVersion        = "X-Skill-Version"
	headerSimpleHostClient    = "X-Simple-Host-Client"
	headerLatestSkillVersion  = "X-Simple-Host-Latest-Skill-Version"
	headerMinimumSkillVersion = "X-Simple-Host-Minimum-Skill-Version"
	headerSkillUpdateURL      = "X-Simple-Host-Update-URL"
)

type strictVersion struct {
	major uint64
	minor uint64
	patch uint64
}

type skillVersionRequiredResponse struct {
	Error                   string `json:"error"`
	Code                    string `json:"code"`
	LatestVersion           string `json:"latest_version"`
	MinimumSupportedVersion string `json:"minimum_supported_version"`
	UpdateURL               string `json:"update_url"`
}

// SkillVersionMiddleware advertises the current skill version and prevents a
// versionless legacy agent from making management decisions with obsolete API
// semantics. It is deliberately streaming-safe: successful responses pass
// through untouched, including their concrete ResponseWriter interfaces.
//
// This is a compatibility guard, not authorization. It must be installed after
// authentication on protected routes so invalid credentials keep their 401
// response. Cookie-only browser requests are unaffected because they do not send
// X-API-Key. First-party UI and intentional non-skill clients identify themselves
// with X-Simple-Host-Client; that classification never grants access.
func SkillVersionMiddleware(serverVersion, minimumVersion string) (func(http.Handler) http.Handler, error) {
	server, ok := parseStrictVersion(serverVersion)
	if !ok {
		return nil, fmt.Errorf("invalid server skill version %q", serverVersion)
	}
	minimum, ok := parseStrictVersion(minimumVersion)
	if !ok {
		return nil, fmt.Errorf("invalid minimum supported skill version %q", minimumVersion)
	}
	if compareStrictVersions(server, minimum) < 0 {
		return nil, fmt.Errorf("server skill version %q is below minimum %q", serverVersion, minimumVersion)
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			setSkillVersionHeaders(w.Header(), serverVersion, minimumVersion)

			// Only API-key management callers participate in the bootstrap guard.
			// Authenticated browser cookies, registration, and public routes remain
			// compatible when this middleware is accidentally or intentionally used.
			if strings.TrimSpace(r.Header.Get("X-API-Key")) == "" || isClassifiedNonSkillClient(r) {
				next.ServeHTTP(w, r)
				return
			}

			client, valid := parseStrictVersion(r.Header.Get(headerSkillVersion))
			if valid && compareStrictVersions(client, minimum) >= 0 {
				next.ServeHTTP(w, r)
				return
			}

			w.Header().Set("Cache-Control", "no-store")
			writeJSON(w, http.StatusBadRequest, skillVersionRequiredResponse{
				Error:                   "a supported Simple Host skill version is required",
				Code:                    "skill_version_required",
				LatestVersion:           serverVersion,
				MinimumSupportedVersion: minimumVersion,
				UpdateURL:               skillUpdateURL,
			})
		})
	}, nil
}

func setSkillVersionHeaders(header http.Header, serverVersion, minimumVersion string) {
	header.Set(headerLatestSkillVersion, serverVersion)
	header.Set(headerMinimumSkillVersion, minimumVersion)
	header.Set(headerSkillUpdateURL, skillUpdateURL)
}

func isClassifiedNonSkillClient(r *http.Request) bool {
	switch strings.ToLower(strings.TrimSpace(r.Header.Get(headerSimpleHostClient))) {
	case "control-ui", "api":
		return true
	default:
		return false
	}
}

func parseStrictVersion(raw string) (strictVersion, bool) {
	parts := strings.Split(strings.TrimSpace(raw), ".")
	if len(parts) != 3 {
		return strictVersion{}, false
	}

	values := make([]uint64, 3)
	for i, part := range parts {
		if part == "" || (len(part) > 1 && part[0] == '0') {
			return strictVersion{}, false
		}
		for _, char := range part {
			if char < '0' || char > '9' {
				return strictVersion{}, false
			}
		}
		value, err := strconv.ParseUint(part, 10, 64)
		if err != nil {
			return strictVersion{}, false
		}
		values[i] = value
	}

	return strictVersion{major: values[0], minor: values[1], patch: values[2]}, true
}

func compareStrictVersions(left, right strictVersion) int {
	if left.major != right.major {
		if left.major < right.major {
			return -1
		}
		return 1
	}
	if left.minor != right.minor {
		if left.minor < right.minor {
			return -1
		}
		return 1
	}
	if left.patch < right.patch {
		return -1
	}
	if left.patch > right.patch {
		return 1
	}
	return 0
}
