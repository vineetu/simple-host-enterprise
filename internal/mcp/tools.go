package mcp

import (
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"strings"
)

// Tool is one callable operation. call turns validated arguments into the REST
// request that performs it; the server serves that request into the existing
// router, so a tool holds no business logic of its own.
type Tool struct {
	Name        string         `json:"name"`
	Title       string         `json:"title,omitempty"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	Annotations map[string]any `json:"annotations,omitempty"`

	call func(args map[string]any) (upstream, error) `json:"-"`

	// family is which part of the surface the tool belongs to. Nothing in the
	// protocol reads it; explainFailure does, so that a team failure is not
	// explained by telling the model to go and look for a site.
	family toolFamily
}

// toolFamily is a string rather than an int so the zero value is "unset" and a
// tool added without one is caught by a test instead of silently inheriting
// whichever family happened to be first.
type toolFamily string

const (
	familyAccount toolFamily = "account"
	familySite    toolFamily = "site"
	familyTeam    toolFamily = "team"
)

// upstream is the REST request a tool resolves to.
type upstream struct {
	Method      string
	Path        string
	Body        []byte
	ContentType string
	// IfMatch carries the caller's ETag so a concurrent deploy is rejected
	// rather than silently overwritten, exactly as on the REST path.
	IfMatch string
	// Fallback is tried when the first attempt returns FallbackOn. One retry,
	// no chain: it exists so a single tool can cover create-or-update.
	FallbackOn int
	Fallback   *upstream
}

// Shared argument wording. A model only knows what these say, and the same
// concept described two ways reads as two concepts.
const (
	siteArgDesc = "Site name as it appears in the URL, e.g. `my-portfolio` — lowercase letters, numbers and hyphens. Not the site's display title."

	ownerArgDesc = "Name of the namespace the site belongs to. A namespace is a person or a team: your own username (from get_account), a team you are in (from list_teams), or the owner shown by list_sites for a site someone shares with you. " +
		"Being in a team grants everything on that team's sites, so a team site is acted on exactly like your own — only the owner differs. A username or team name, never a display name or email."

	teamArgDesc = "Team name as it appears in the URL, e.g. `acme-team` — lowercase letters, numbers and hyphens. Get it from list_teams; a team you are not in is indistinguishable from one that does not exist, so do not guess."

	etagArgDesc = "The site's ETag as it was when you started editing — capture it with get_site before making any change and keep it with the working copy. " +
		"Required when anyone else can deploy the site (it has editors, or a team owns it), optional otherwise. " +
		"Do NOT refresh it just before deploying: a fresh ETag hides a change somebody else made instead of catching it, which is the one thing it exists to do."
)

func object(properties map[string]any, required ...string) map[string]any {
	schema := map[string]any{"type": "object", "properties": properties}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

// noArgs is the spec's recommended schema for a tool that takes nothing: it
// accepts only an empty object rather than silently tolerating junk.
func noArgs() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}
}

func str(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

// readOnly and destructive mark behaviour a client may surface before calling.
func readOnly() map[string]any {
	return map[string]any{"readOnlyHint": true, "destructiveHint": false, "idempotentHint": true, "openWorldHint": false}
}

func writes(destructive bool) map[string]any {
	return map[string]any{"readOnlyHint": false, "destructiveHint": destructive, "openWorldHint": false}
}

func stringArg(args map[string]any, key string) (string, error) {
	raw, present := args[key]
	if !present {
		return "", fmt.Errorf("%s is required", key)
	}
	value, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("%s must be a string, got %T", key, raw)
	}
	if strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("%s must not be empty", key)
	}
	return value, nil
}

// optionalString returns an absent argument as "", but a present-and-wrong one
// as an error.
//
// Returning "" for a non-string is what made two silent-corruption bugs
// possible: an `encoding` of 7 fell through to "treat as text" and wrote base64
// into the file verbatim, and an `etag` of 7 disabled the concurrency guard
// while the caller believed it was protected. A wrong type is now refused.
func optionalString(args map[string]any, key string) (string, error) {
	raw, present := args[key]
	if !present || raw == nil {
		return "", nil
	}
	value, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("%s must be a string, got %T", key, raw)
	}
	return value, nil
}

// wholeNumber reads an integer argument. JSON has one number type, so a
// fractional value arrives as a valid float and silently truncated: version 1.9
// rolled a site back to v1 and reported success.
func wholeNumber(args map[string]any, key string) (int, error) {
	raw, present := args[key]
	if !present {
		return 0, fmt.Errorf("%s is required", key)
	}
	value, ok := raw.(float64)
	if !ok {
		return 0, fmt.Errorf("%s must be a number, got %T", key, raw)
	}
	if value != math.Trunc(value) {
		return 0, fmt.Errorf("%s must be a whole number, got %v", key, value)
	}
	if value < 1 || value > math.MaxInt32 {
		return 0, fmt.Errorf("%s must be between 1 and %d, got %v", key, math.MaxInt32, value)
	}
	return int(value), nil
}

// boolArg refuses the truthy-looking values a model might send instead of a
// boolean, rather than guessing which way the user meant it.
func boolArg(args map[string]any, key string) (bool, error) {
	raw, present := args[key]
	if !present {
		return false, fmt.Errorf("%s is required", key)
	}
	value, ok := raw.(bool)
	if !ok {
		return false, fmt.Errorf("%s must be true or false, got %T", key, raw)
	}
	return value, nil
}

// segment guards a path segment before it is interpolated into a URL. The REST
// layer validates these again; doing it here turns a rejected request into a
// precise message the model can correct.
func segment(value, field string) (string, error) {
	if strings.ContainsAny(value, "/?#") || value == "." || value == ".." {
		return "", fmt.Errorf("%s must be a single path segment, got %q", field, value)
	}
	return url.PathEscape(value), nil
}

// siteRoute resolves the pair of routes a site operation can take.
//
// A site the caller owns is reachable at /api/sites/{site}. A site shared with
// them is only reachable at /api/collaboration/sites/{owner}/{site}, which also
// accepts the owner — so when an owner is named, that is the route used for
// both. Getting this wrong is not a 404: a caller who owns a site of the same
// name would silently deploy over their own.
func siteRoute(args map[string]any, suffix string) (ownerScoped string, collaboration string, err error) {
	site, err := stringArg(args, "site")
	if err != nil {
		return "", "", err
	}
	siteSeg, err := segment(site, "site")
	if err != nil {
		return "", "", err
	}
	ownerScoped = "/api/sites/" + siteSeg
	if suffix != "" {
		ownerScoped += "/" + suffix
	}

	owner, err := optionalString(args, "owner")
	if err != nil {
		return "", "", err
	}
	if strings.TrimSpace(owner) == "" {
		return ownerScoped, "", nil
	}
	ownerSeg, err := segment(owner, "owner")
	if err != nil {
		return "", "", err
	}
	collaboration = "/api/collaboration/sites/" + ownerSeg + "/" + siteSeg
	if suffix != "" {
		collaboration += "/" + suffix
	}
	return ownerScoped, collaboration, nil
}

// teamRoute builds a team route from the `team` argument. A team has one
// route shape and no owner-scoped alternative, because a team is the owner.
func teamRoute(args map[string]any, suffix string) (string, error) {
	team, err := stringArg(args, "team")
	if err != nil {
		return "", err
	}
	teamSeg, err := segment(team, "team")
	if err != nil {
		return "", err
	}
	path := "/api/teams/" + teamSeg
	if suffix != "" {
		path += "/" + suffix
	}
	return path, nil
}

// deploySiteSchema is built rather than declared so the conditional
// requirement can be stated: intent is required exactly when owner is, which
// a plain required list cannot say. The call func enforces it regardless —
// dependentRequired is a courtesy to clients that validate before sending.
func deploySiteSchema() map[string]any {
	schema := object(map[string]any{
		"site": str(siteArgDesc + " With intent create and no owner, the site is created in your own account."),
		"files": map[string]any{
			"type": "array",
			"description": "The complete file list for the new version — a full replacement, not a patch. " +
				"Must include a file whose path is exactly `index.html`, or the deploy is rejected. " +
				"Total decoded content must stay under 8 MiB.",
			"minItems": 1,
			"items": object(map[string]any{
				"path":    str("Relative path from the site root, e.g. `index.html` or `assets/app.css`. No leading slash and no `..`."),
				"content": str("The file's contents. Plain text by default; set encoding to base64 for images and other binary files."),
				"encoding": map[string]any{
					"type":        "string",
					"enum":        []string{"text", "base64"},
					"description": "How content is encoded. Defaults to text.",
				},
			}, "path", "content"),
		},
		"intent": map[string]any{
			"type": "string",
			"enum": []string{"create", "update"},
			"description": "Which of the two you are doing: `create` for a site that does not exist in that namespace yet, `update` to publish a new version of one that does. " +
				"Required whenever owner is given. Settle it with get_site or list_sites before calling — do not send `create` speculatively and read the failure, " +
				"because a create that lands in the wrong namespace is a second site nobody is looking at.",
		},
		"owner": str(ownerArgDesc + " Required to publish into a team's namespace or to a site shared with you."),
		"etag":  str(etagArgDesc),
	}, "site", "files")
	schema["dependentRequired"] = map[string]any{"owner": []string{"intent"}}
	return schema
}

// Tools is the callable surface. The order is the order a first-time agent
// needs them, and it is fixed so clients can cache the list.
func Tools() []Tool {
	return []Tool{
		{
			Name:  "get_account",
			Title: "Get the signed-in account",
			Description: "Return the authenticated Simple Host account: its username, whether it is an admin, and the teams it belongs to. " +
				"The username and each team's `name` are the `owner` values the other tools take — a site lives in one of those namespaces and nowhere else. " +
				"Each team also has an `id` that never changes, which is what to record when binding a project to a namespace: a name can be released and registered by somebody else, an id cannot. " +
				"Each site's address is the `url` that list_sites and deploy_site return; use that rather than composing one from a name. " +
				"Call this when you need the owner name and do not already have it.",
			InputSchema: noArgs(),
			Annotations: readOnly(),
			family:      familyAccount,
			call: func(map[string]any) (upstream, error) {
				return upstream{Method: "GET", Path: "/api/me"}, nil
			},
		},
		{
			Name:  "list_sites",
			Title: "List sites I can act on",
			Description: "List every site the account can act on: sites it owns, sites owned by a team it belongs to, and sites shared with it as an editor. " +
				"Each entry gives the site name, its owner, its `access_role` — owner, member (the site belongs to a team you are in) or editor — and its address: " +
				"`url` is absolute, and `public_path` is the address to hand out, " +
				"which may be absolute rather than a path, so use it exactly as returned and never prefix it with the server origin. " +
				"Call this first when acting on a site that already exists — " +
				"it is the only way to learn the exact site name and the owner other tools need.",
			InputSchema: noArgs(),
			Annotations: readOnly(),
			family:      familySite,
			call: func(map[string]any) (upstream, error) {
				return upstream{Method: "GET", Path: "/api/collaboration/sites"}, nil
			},
		},
		{
			Name:  "get_site",
			Title: "Get one site",
			Description: "Fetch one site's current state: its live version number, whether it is listed publicly, its address, and the ETag needed to change it safely. " +
				"`url` is absolute and `public_path` is the address to hand out; it may be absolute rather than a path, so use it exactly as returned. " +
				"Call this before deploy_site or rollback_site on an existing site, and pass the returned etag to that call.",
			InputSchema: object(map[string]any{
				"site":  str(siteArgDesc),
				"owner": str(ownerArgDesc),
			}, "site", "owner"),
			Annotations: readOnly(),
			family:      familySite,
			call: func(args map[string]any) (upstream, error) {
				_, collaboration, err := siteRoute(args, "")
				if err != nil {
					return upstream{}, err
				}
				if collaboration == "" {
					return upstream{}, fmt.Errorf("owner is required; call get_account for your own username, list_teams for a team's name, or list_sites for the owner of a site shared with you")
				}
				return upstream{Method: "GET", Path: collaboration}, nil
			},
		},
		{
			Name:  "deploy_site",
			Title: "Publish a site",
			Description: "Publish a website from files given inline, either creating the site or publishing a new version of one that already exists. " +
				"Say which with intent: creating and updating are separate intentions, and by the time you call this you have already settled which one you mean — " +
				"a create that finds the site already there is an error to report, not something to paper over. " +
				"Earlier versions are retained and can be restored with rollback_site. " +
				"This REPLACES the whole site: the files given are the complete new version, and anything not listed stops existing. " +
				"To change one page of an existing site you must send every other file again unchanged. " +
				"Call list_sites or get_site first to settle which namespace you are publishing into and whether the site exists there; " +
				"pass that namespace as owner, and for an update pass the etag you captured before you started editing.",
			InputSchema: deploySiteSchema(),
			Annotations: writes(false),
			family:      familySite,
			call: func(args map[string]any) (upstream, error) {
				ownerScoped, collaboration, err := siteRoute(args, "")
				if err != nil {
					return upstream{}, err
				}
				archive, err := buildArchive(args)
				if err != nil {
					return upstream{}, err
				}
				etag, err := optionalString(args, "etag")
				if err != nil {
					return upstream{}, err
				}
				intent, err := optionalString(args, "intent")
				if err != nil {
					return upstream{}, err
				}
				switch intent {
				case "", "create", "update":
				default:
					return upstream{}, fmt.Errorf("intent must be create or update, got %q", intent)
				}

				path := ownerScoped
				if collaboration != "" {
					path = collaboration
					if intent == "" {
						return upstream{}, fmt.Errorf("intent is required when owner is set: call get_site for %q first, then pass create if the site is not there or update if it is", args["owner"])
					}
				}

				// One intent, one request. Nothing converts a create into an
				// update or the reverse: a named intent that meets the wrong
				// reality is a refusal the caller has to read, because the
				// alternative is a second site in a namespace nobody meant.
				switch intent {
				case "create":
					return upstream{Method: "POST", Path: path, Body: archive, ContentType: "application/gzip"}, nil
				case "update":
					return upstream{Method: "PUT", Path: path, Body: archive, ContentType: "application/gzip", IfMatch: etag}, nil
				}

				// No owner and no intent: a pre-0.9 client, which had neither
				// argument and meant "publish this" by a create it expected to
				// fall back. Kept for those callers and reachable no other way.
				update := &upstream{Method: "PUT", Path: ownerScoped, Body: archive,
					ContentType: "application/gzip", IfMatch: etag}
				return upstream{
					Method: "POST", Path: ownerScoped, Body: archive, ContentType: "application/gzip",
					FallbackOn: 409, Fallback: update,
				}, nil
			},
		},
		{
			Name:  "list_site_versions",
			Title: "List a site's versions",
			Description: "List the versions retained for a site, newest first, with each version's number, status and when it was created. " +
				"Call this before rollback_site so the target version is chosen from real values rather than guessed.",
			InputSchema: object(map[string]any{
				"site":  str(siteArgDesc),
				"owner": str(ownerArgDesc + " Omit only for a site in your own account."),
			}, "site"),
			Annotations: readOnly(),
			family:      familySite,
			call: func(args map[string]any) (upstream, error) {
				ownerScoped, collaboration, err := siteRoute(args, "versions")
				if err != nil {
					return upstream{}, err
				}
				if collaboration != "" {
					return upstream{Method: "GET", Path: collaboration}, nil
				}
				return upstream{Method: "GET", Path: ownerScoped}, nil
			},
		},
		{
			Name:  "rollback_site",
			Title: "Roll back to an earlier version",
			Description: "Make one of a site's earlier versions live again. This changes what visitors see immediately. " +
				"Call list_site_versions first and confirm the target version with the user before calling this. " +
				"Nothing is destroyed: the version being replaced stays retained and can be restored the same way.",
			InputSchema: object(map[string]any{
				"site":    str(siteArgDesc),
				"version": map[string]any{"type": "integer", "description": "Version number to make live — the `version_number` value from list_site_versions, as a number (3, not \"v3\")."},
				"owner":   str(ownerArgDesc + " Omit only for a site in your own account."),
				"etag":    str(etagArgDesc),
			}, "site", "version"),
			Annotations: writes(true),
			family:      familySite,
			call: func(args map[string]any) (upstream, error) {
				ownerScoped, collaboration, err := siteRoute(args, "rollback")
				if err != nil {
					return upstream{}, err
				}
				number, err := wholeNumber(args, "version")
				if err != nil {
					return upstream{}, fmt.Errorf("%w; call list_site_versions to see valid version numbers", err)
				}
				etag, err := optionalString(args, "etag")
				if err != nil {
					return upstream{}, err
				}
				body, _ := json.Marshal(map[string]any{"version": number})
				path := ownerScoped
				if collaboration != "" {
					path = collaboration
				}
				return upstream{Method: "POST", Path: path, Body: body,
					ContentType: "application/json", IfMatch: etag}, nil
			},
		},
		{
			Name:  "set_site_listing",
			Title: "List or unlist a site in the showcase",
			Description: "Control whether a site appears in Simple Host's public showcase. " +
				"This does NOT control who can read the site: an unlisted site is still served to anyone who has its URL. " +
				"There is no way to restrict access to a site through these tools, so do not describe an unlisted site as private. " +
				"Works on a site you own and on a site owned by a team you are in; an editor a site is shared with cannot change its listing.",
			InputSchema: object(map[string]any{
				"site":   str(siteArgDesc),
				"owner":  str(ownerArgDesc + " Omit only for a site in your own account."),
				"listed": map[string]any{"type": "boolean", "description": "true shows the site in the public showcase; false removes it from the showcase. Either way the site stays readable to anyone with the URL."},
			}, "site", "listed"),
			Annotations: writes(false),
			family:      familySite,
			call: func(args map[string]any) (upstream, error) {
				ownerScoped, collaboration, err := siteRoute(args, "visibility")
				if err != nil {
					return upstream{}, err
				}
				listed, err := boolArg(args, "listed")
				if err != nil {
					return upstream{}, err
				}
				body, _ := json.Marshal(map[string]any{"public": listed})
				path := ownerScoped
				if collaboration != "" {
					path = collaboration
				}
				return upstream{Method: "POST", Path: path, Body: body, ContentType: "application/json"}, nil
			},
		},
		{
			Name:  "list_site_editors",
			Title: "List a site's editors",
			Description: "List the users a site has been shared with — the people who can deploy and roll back it without owning it. " +
				"Members of a team that owns the site are not editors and are not listed here; call list_team_members for those.",
			InputSchema: object(map[string]any{
				"site":  str(siteArgDesc),
				"owner": str(ownerArgDesc),
			}, "site", "owner"),
			Annotations: readOnly(),
			family:      familySite,
			call: func(args map[string]any) (upstream, error) {
				return collaborationSuffix(args, "editors", "GET", nil)
			},
		},
		{
			Name:  "find_users",
			Title: "Find users to share with",
			Description: "Search registered Simple Host usernames. Use this to turn a person's name into the exact username grant_site_editor needs — do not guess a username. " +
				"This searches people to share ONE site with; to add somebody to a team, call find_team_members instead.",
			InputSchema: object(map[string]any{
				"site":  str(siteArgDesc + " The site you intend to share."),
				"owner": str(ownerArgDesc),
				"query": str("Part of a username to search for, e.g. `bob`."),
			}, "site", "owner", "query"),
			Annotations: readOnly(),
			family:      familySite,
			call: func(args map[string]any) (upstream, error) {
				query, err := stringArg(args, "query")
				if err != nil {
					return upstream{}, err
				}
				up, err := collaborationSuffix(args, "editor-candidates", "GET", nil)
				if err != nil {
					return upstream{}, err
				}
				up.Path += "?q=" + url.QueryEscape(query) + "&limit=20"
				return up, nil
			},
		},
		{
			Name:  "grant_site_editor",
			Title: "Share a site with someone",
			Description: "Give registered users permission to deploy and roll back one site. They keep the site's existing URL and cannot delete it, change its listing, or change who else has access. " +
				"Works on a site you own and on a site owned by a team you are in. " +
				"Usernames must be exact — call find_users first if you only know a person's name. " +
				"This shares one site; it does not put anybody in a team.",
			InputSchema: object(map[string]any{
				"site":  str(siteArgDesc),
				"owner": str(ownerArgDesc),
				"usernames": map[string]any{
					"type":        "array",
					"description": "Exact usernames to grant editor access to.",
					"minItems":    1,
					"items":       map[string]any{"type": "string"},
				},
			}, "site", "owner", "usernames"),
			Annotations: writes(false),
			family:      familySite,
			call: func(args map[string]any) (upstream, error) {
				names, err := stringList(args, "usernames")
				if err != nil {
					return upstream{}, err
				}
				body, _ := json.Marshal(map[string]any{"usernames": names})
				return collaborationSuffix(args, "editors", "POST", body)
			},
		},
		{
			Name:  "revoke_site_editor",
			Title: "Stop sharing a site with someone",
			Description: "Remove one user's editor access to a site. They keep nothing they have already downloaded, and versions they deployed stay live until you roll back. " +
				"This cannot remove somebody who reaches the site by being in the team that owns it — for that, call remove_team_member.",
			InputSchema: object(map[string]any{
				"site":     str(siteArgDesc),
				"owner":    str(ownerArgDesc),
				"username": str("Exact username to remove, as shown by list_site_editors."),
			}, "site", "owner", "username"),
			Annotations: writes(true),
			family:      familySite,
			call: func(args map[string]any) (upstream, error) {
				username, err := stringArg(args, "username")
				if err != nil {
					return upstream{}, err
				}
				userSeg, err := segment(username, "username")
				if err != nil {
					return upstream{}, err
				}
				return collaborationSuffix(args, "editors/"+userSeg, "DELETE", nil)
			},
		},
		{
			Name:  "delete_site",
			Title: "Delete a site permanently",
			Description: "Permanently delete a site and every version of it. This cannot be undone and the URL stops working immediately. " +
				"Works on a site you own and on a site owned by a team you are in; an editor a site is shared with cannot delete it. " +
				"Always confirm with the user before calling this. " +
				"There is no way to delete a single version — use rollback_site to stop serving an unwanted one.",
			InputSchema: object(map[string]any{
				"site":  str(siteArgDesc),
				"owner": str(ownerArgDesc + " Omit only for a site in your own account."),
			}, "site"),
			Annotations: writes(true),
			family:      familySite,
			call: func(args map[string]any) (upstream, error) {
				ownerScoped, collaboration, err := siteRoute(args, "")
				if err != nil {
					return upstream{}, err
				}
				if collaboration != "" {
					return upstream{Method: "DELETE", Path: collaboration}, nil
				}
				return upstream{Method: "DELETE", Path: ownerScoped}, nil
			},
		},
		{
			Name:  "create_team",
			Title: "Create a team",
			Description: "Create a team. A team is a namespace that owns sites exactly as a person does, but it is not a person: it has no API key, and its members act with their own. " +
				"You become its first member. There is one role and no other: everybody in a team may publish, roll back, relist and delete any of the team's sites, add and remove members, and delete the team. " +
				"Call this ONLY when the user asks for a team. Creating one is never a step on the way to publishing something, and never the answer to a deploy that failed.",
			InputSchema: object(map[string]any{
				"name": str("The team's name, e.g. `acme-team` — lowercase letters, numbers and hyphens, no dots. " +
					"It becomes part of the team's web address and cannot be changed afterwards, so use the name the user gave rather than one you compose from it."),
			}, "name"),
			Annotations: writes(false),
			family:      familyTeam,
			call: func(args map[string]any) (upstream, error) {
				name, err := stringArg(args, "name")
				if err != nil {
					return upstream{}, err
				}
				body, _ := json.Marshal(map[string]any{"name": name})
				return upstream{Method: "POST", Path: "/api/teams", Body: body, ContentType: "application/json"}, nil
			},
		},
		{
			Name:  "list_teams",
			Title: "List my teams",
			Description: "List the teams this account belongs to, with each team's name and unchanging id — the same list get_account returns, on its own. " +
				"A team's name is the `owner` value the site tools take for that team's sites. " +
				"Call this before create_team: a team the user already has is the thing a second team under a near-identical name would quietly replace.",
			InputSchema: noArgs(),
			Annotations: readOnly(),
			family:      familyTeam,
			call: func(map[string]any) (upstream, error) {
				return upstream{Method: "GET", Path: "/api/teams"}, nil
			},
		},
		{
			Name:  "list_team_members",
			Title: "List a team's members",
			Description: "List the people in a team and when each of them joined. " +
				"Everybody listed has the same powers over the team and over every site it owns; there are no roles to compare.",
			InputSchema: object(map[string]any{
				"team": str(teamArgDesc),
			}, "team"),
			Annotations: readOnly(),
			family:      familyTeam,
			call: func(args map[string]any) (upstream, error) {
				path, err := teamRoute(args, "members")
				if err != nil {
					return upstream{}, err
				}
				return upstream{Method: "GET", Path: path}, nil
			},
		},
		{
			Name:  "find_team_members",
			Title: "Find people to add to a team",
			Description: "Search registered Simple Host people to add to a team. Use this to turn a person's name into the exact username add_team_member needs — do not guess a username. " +
				"Each result says whether that person is already a member.",
			InputSchema: object(map[string]any{
				"team":  str(teamArgDesc),
				"query": str("Part of a username to search for, e.g. `bob`."),
			}, "team", "query"),
			Annotations: readOnly(),
			family:      familyTeam,
			call: func(args map[string]any) (upstream, error) {
				query, err := stringArg(args, "query")
				if err != nil {
					return upstream{}, err
				}
				path, err := teamRoute(args, "member-candidates")
				if err != nil {
					return upstream{}, err
				}
				return upstream{Method: "GET", Path: path + "?q=" + url.QueryEscape(query) + "&limit=20"}, nil
			},
		},
		{
			Name:  "add_team_member",
			Title: "Add people to a team",
			Description: "Add registered people to a team. There is no lesser role to add somebody as: everyone added can immediately publish over, roll back and delete every site the team owns, add and remove other members, and delete the team. " +
				"Confirm with the user before adding anyone. Usernames must be exact — call find_team_members first if you only know a person's name.",
			InputSchema: object(map[string]any{
				"team": str(teamArgDesc),
				"usernames": map[string]any{
					"type":        "array",
					"description": "Exact usernames to add to the team, as shown by find_team_members.",
					"minItems":    1,
					"items":       map[string]any{"type": "string"},
				},
			}, "team", "usernames"),
			Annotations: writes(false),
			family:      familyTeam,
			call: func(args map[string]any) (upstream, error) {
				names, err := stringList(args, "usernames")
				if err != nil {
					return upstream{}, err
				}
				path, err := teamRoute(args, "members")
				if err != nil {
					return upstream{}, err
				}
				body, _ := json.Marshal(map[string]any{"usernames": names})
				return upstream{Method: "POST", Path: path, Body: body, ContentType: "application/json"}, nil
			},
		},
		{
			Name:  "remove_team_member",
			Title: "Remove somebody from a team",
			Description: "Remove one person from a team. They lose access to every site the team owns immediately, but versions they deployed stay live until somebody rolls them back. " +
				"A team always keeps at least one member, so removing the last one is refused — winding a team up is delete_team, after its sites are gone.",
			InputSchema: object(map[string]any{
				"team":     str(teamArgDesc),
				"username": str("Exact username to remove, as shown by list_team_members."),
			}, "team", "username"),
			Annotations: writes(true),
			family:      familyTeam,
			call: func(args map[string]any) (upstream, error) {
				username, err := stringArg(args, "username")
				if err != nil {
					return upstream{}, err
				}
				userSeg, err := segment(username, "username")
				if err != nil {
					return upstream{}, err
				}
				path, err := teamRoute(args, "members/"+userSeg)
				if err != nil {
					return upstream{}, err
				}
				return upstream{Method: "DELETE", Path: path}, nil
			},
		},
		{
			Name:  "delete_team",
			Title: "Delete a team",
			Description: "Permanently delete a team. Only possible once the team owns no sites, so this is never a way to delete its sites — delete those first, each one confirmed with the user. " +
				"The name is released once the team is gone and somebody else may register it, so this cannot be undone. Always confirm with the user before calling this.",
			InputSchema: object(map[string]any{
				"team": str(teamArgDesc),
			}, "team"),
			Annotations: writes(true),
			family:      familyTeam,
			call: func(args map[string]any) (upstream, error) {
				path, err := teamRoute(args, "")
				if err != nil {
					return upstream{}, err
				}
				return upstream{Method: "DELETE", Path: path}, nil
			},
		},
	}
}

func collaborationSuffix(args map[string]any, suffix, method string, body []byte) (upstream, error) {
	_, collaboration, err := siteRoute(args, suffix)
	if err != nil {
		return upstream{}, err
	}
	if collaboration == "" {
		return upstream{}, fmt.Errorf("owner is required; call get_account for your own username, list_teams for a team's name, or list_sites for the owner of a site shared with you")
	}
	up := upstream{Method: method, Path: collaboration}
	if body != nil {
		up.Body = body
		up.ContentType = "application/json"
	}
	return up, nil
}

// stringList reads a non-empty array of non-empty strings. Two tools take one,
// and a model that sends [""] or ["bob", 7] should be told which element is
// wrong rather than have it reach the API as a name.
func stringList(args map[string]any, key string) ([]string, error) {
	raw, present := args[key]
	if !present {
		return nil, fmt.Errorf("%s is required", key)
	}
	list, ok := raw.([]any)
	if !ok || len(list) == 0 {
		return nil, fmt.Errorf("%s must be a non-empty array of strings", key)
	}
	values := make([]string, 0, len(list))
	for i, entry := range list {
		value, ok := entry.(string)
		if !ok || strings.TrimSpace(value) == "" {
			return nil, fmt.Errorf("%s[%d] must be a non-empty string", key, i)
		}
		values = append(values, value)
	}
	return values, nil
}
