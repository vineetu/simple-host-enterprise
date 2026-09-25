package storage

import (
	"fmt"
	"strconv"
)

// Object keys. Every key is built here from a site id and either a version
// number or an asset id, and each of those is validated to a fixed shape
// before it is used: a lowercase canonical UUID, or a positive decimal
// integer. A key can therefore only ever name something beneath its own
// site's prefix — no request-supplied path, file name, username or site name
// ever becomes part of a key. File paths inside a version live inside the
// version's archive, and are confined when the archive is unpacked
// (archive.go), not by the key.
//
// Layout, relative to the configured bucket prefix:
//
//	sites/<site-id>/v<N>.tar.gz      one immutable deployed version
//	sites/<site-id>/assets/<id>      one immutable uploaded asset
//
// Keying by site id rather than owner and site name means a deleted and
// re-created site never shares a key with its predecessor, so retiring the
// old site's prefix can never touch the new one's objects.

// SitePrefix is the prefix holding everything a site owns.
func SitePrefix(siteID string) (string, error) {
	if !isUUID(siteID) {
		return "", fmt.Errorf("invalid site id %q", siteID)
	}
	return "sites/" + siteID + "/", nil
}

// VersionKey names one deployed version's archive.
func VersionKey(siteID string, version int) (string, error) {
	prefix, err := SitePrefix(siteID)
	if err != nil {
		return "", err
	}
	if version < 1 {
		return "", fmt.Errorf("invalid version %d", version)
	}
	return prefix + "v" + strconv.Itoa(version) + ".tar.gz", nil
}

func assetKey(siteID, assetID string) (string, error) {
	prefix, err := SitePrefix(siteID)
	if err != nil {
		return "", err
	}
	if !isUUID(assetID) {
		return "", ErrAssetNotFound
	}
	return prefix + "assets/" + assetID, nil
}

// isUUID accepts exactly the lowercase hyphenated form Postgres returns for a
// uuid column: 8-4-4-4-12 hex digits.
func isUUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
				return false
			}
		}
	}
	return true
}
