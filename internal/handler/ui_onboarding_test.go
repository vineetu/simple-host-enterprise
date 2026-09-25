package handler

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"testing"

	htmlnode "golang.org/x/net/html"
)

func TestOnboardingPagesShareAccessiblePrimaryNavigation(t *testing.T) {
	t.Parallel()

	type navigationLink struct {
		label string
		href  string
	}
	wantLinks := []navigationLink{
		{label: "Showcase", href: "/showcase"},
		{label: "Capabilities", href: "/capabilities.html"},
		{label: "What's New", href: "/changelog.html"},
		{label: "Install", href: "/install.html"},
		{label: "API", href: "/docs.html"},
	}

	pages := []struct {
		name        string
		document    string
		currentHref string
	}{
		{name: "Home", document: readOnboardingStaticFile(t, "index.html"), currentHref: "/"},
		{name: "Capabilities", document: readOnboardingStaticFile(t, "capabilities.html"), currentHref: "/capabilities.html"},
		{name: "What's New", document: readOnboardingStaticFile(t, "changelog.html"), currentHref: "/changelog.html"},
		{name: "Install", document: readOnboardingStaticFile(t, "install.html"), currentHref: "/install.html"},
		{name: "API", document: readOnboardingStaticFile(t, "docs.html"), currentHref: "/docs.html"},
		{name: "Showcase", document: renderOnboardingShowcase(t), currentHref: "/showcase"},
	}

	for _, page := range pages {
		page := page
		t.Run(page.name, func(t *testing.T) {
			document := parseOnboardingHTML(t, page.document)

			var headerStylesheets []*htmlnode.Node
			for _, link := range onboardingElements(document, "link") {
				if href, _ := onboardingAttribute(link, "href"); href == "/site-header.css" {
					headerStylesheets = append(headerStylesheets, link)
				}
			}
			if len(headerStylesheets) != 1 {
				t.Fatalf("found %d /site-header.css links, want 1", len(headerStylesheets))
			}
			if rel, _ := onboardingAttribute(headerStylesheets[0], "rel"); rel != "stylesheet" {
				t.Errorf("/site-header.css rel = %q, want stylesheet", rel)
			}

			var primaryNavs []*htmlnode.Node
			for _, nav := range onboardingElements(document, "nav") {
				if label, _ := onboardingAttribute(nav, "aria-label"); label == "Primary navigation" {
					primaryNavs = append(primaryNavs, nav)
				}
			}
			if len(primaryNavs) != 1 {
				t.Fatalf("found %d Primary navigation landmarks, want 1", len(primaryNavs))
			}

			links := onboardingElements(primaryNavs[0], "a")
			if len(links) != len(wantLinks) {
				t.Fatalf("primary navigation has %d links, want %d", len(links), len(wantLinks))
			}
			uniqueLinks := make(map[navigationLink]struct{}, len(links))
			for i, want := range wantLinks {
				got := navigationLink{label: onboardingText(links[i])}
				got.href, _ = onboardingAttribute(links[i], "href")
				uniqueLinks[got] = struct{}{}
				if got != want {
					t.Errorf("primary navigation link %d = %#v, want %#v", i, got, want)
				}
			}
			if len(uniqueLinks) != len(wantLinks) {
				t.Errorf("primary navigation has %d unique links, want %d", len(uniqueLinks), len(wantLinks))
			}

			// Only page markers count. aria-current is also the correct marker
			// for a selected filter elsewhere on a page — the showcase's type
			// chips use aria-current="true" — and this test is about which
			// page the nav says you are on.
			var current []*htmlnode.Node
			for _, candidate := range onboardingElementsWithAttribute(document, "aria-current") {
				if value, _ := onboardingAttribute(candidate, "aria-current"); value == "page" {
					current = append(current, candidate)
				}
			}
			if len(current) != 1 {
				t.Fatalf("found %d aria-current elements, want 1", len(current))
			}
			if value, _ := onboardingAttribute(current[0], "aria-current"); value != "page" {
				t.Errorf("aria-current = %q, want page", value)
			}
			if href, _ := onboardingAttribute(current[0], "href"); href != page.currentHref {
				t.Errorf("current-page href = %q, want %q", href, page.currentHref)
			}

			var skipLinks []*htmlnode.Node
			for _, anchor := range onboardingElements(document, "a") {
				href, _ := onboardingAttribute(anchor, "href")
				if href == "#main-content" && onboardingText(anchor) == "Skip to main content" {
					skipLinks = append(skipLinks, anchor)
				}
			}
			if len(skipLinks) != 1 {
				t.Fatalf("found %d skip links to #main-content, want 1", len(skipLinks))
			}

			main := onboardingElementsByID(document, "main-content")
			if len(main) != 1 || main[0].Data != "main" {
				t.Fatalf("found %d main-content elements, want one <main>", len(main))
			}
			if tabindex, _ := onboardingAttribute(main[0], "tabindex"); tabindex != "-1" {
				t.Errorf("main-content tabindex = %q, want -1", tabindex)
			}
		})
	}
}

func TestOnboardingAPIDocsPreserveSwaggerBootstrap(t *testing.T) {
	t.Parallel()

	documentText := readOnboardingStaticFile(t, "docs.html")
	document := parseOnboardingHTML(t, documentText)

	var viewports []*htmlnode.Node
	for _, meta := range onboardingElements(document, "meta") {
		if name, _ := onboardingAttribute(meta, "name"); strings.EqualFold(name, "viewport") {
			viewports = append(viewports, meta)
		}
	}
	if len(viewports) != 1 {
		t.Fatalf("found %d viewport declarations, want 1", len(viewports))
	}
	viewport, _ := onboardingAttribute(viewports[0], "content")
	for _, want := range []string{"width=device-width", "initial-scale=1.0"} {
		if !strings.Contains(viewport, want) {
			t.Errorf("viewport %q is missing %q", viewport, want)
		}
	}

	if !onboardingHasElementWithAttribute(document, "link", "href", "https://unpkg.com/swagger-ui-dist@5/swagger-ui.css") {
		t.Error("Swagger UI stylesheet is missing")
	}
	if !onboardingHasElementWithAttribute(document, "script", "src", "https://unpkg.com/swagger-ui-dist@5/swagger-ui-bundle.js") {
		t.Error("Swagger UI bundle is missing")
	}
	if len(onboardingElementsByID(document, "swagger-ui")) != 1 {
		t.Error("Swagger UI mount point is missing or duplicated")
	}

	for _, pattern := range []string{
		`SwaggerUIBundle\s*\(\s*\{`,
		`url\s*:\s*['"]/openapi\.yaml['"]`,
		`dom_id\s*:\s*['"]#swagger-ui['"]`,
		`presets\s*:`,
		`deepLinking\s*:\s*true`,
	} {
		if !regexp.MustCompile(pattern).MatchString(documentText) {
			t.Errorf("Swagger bootstrap is missing pattern %q", pattern)
		}
	}
}

func TestHomepageOnboardingJourneyAndEmailPrivacy(t *testing.T) {
	t.Parallel()

	homepage := readOnboardingStaticFile(t, "index.html")
	document := parseOnboardingHTML(t, homepage)

	hero := onboardingSingleElementWithClass(t, document, "section", "home-hero")
	// The headline carries speed, not breadth: a site here takes minutes, and
	// the eyebrow no longer repeats the wordmark already in the header.
	assertOnboardingElementText(t, hero, "h1", "Build and share a website in about 20 minutes")
	heroEyebrows := onboardingElementsWithClass(hero, "p", "home-eyebrow")
	if len(heroEyebrows) == 1 && onboardingText(heroEyebrows[0]) == "Simple Host" {
		t.Error("hero eyebrow still duplicates the Simple Host wordmark already in the header")
	}
	heroLinks := onboardingElements(hero, "a")
	if len(heroLinks) != 2 {
		t.Fatalf("hero has %d links, want 2", len(heroLinks))
	}
	wantHeroLinks := []struct {
		text string
		href string
	}{
		{text: "Get started", href: "#get-started"},
		{text: "Browse examples", href: "/showcase"},
	}
	for i, want := range wantHeroLinks {
		href, _ := onboardingAttribute(heroLinks[i], "href")
		if got := onboardingText(heroLinks[i]); got != want.text || href != want.href {
			t.Errorf("hero link %d = text %q href %q, want text %q href %q", i, got, href, want.text, want.href)
		}
	}

	// The journey is now four steps revealed one at a time. Steps 2-4 ship
	// hidden so a first-timer sees one decision; step 1 ships visible so a
	// no-JS visitor still reaches the install prompt.
	journey := onboardingSingleElementWithClass(t, document, "ol", "journey__steps")
	steps := onboardingDirectChildren(journey, "li")
	if len(steps) != 4 {
		t.Fatalf("onboarding journey has %d direct steps, want 4", len(steps))
	}
	gotStepTitles := make([]string, 0, len(steps))
	for _, step := range steps {
		headings := onboardingElements(step, "h3")
		if len(headings) == 0 {
			t.Fatal("onboarding step has no h3")
		}
		gotStepTitles = append(gotStepTitles, onboardingText(headings[0]))
	}
	wantStepTitles := []string{"Install Simple Host", "Describe what you want", "Let it work", "Find it and share it"}
	if !slices.Equal(gotStepTitles, wantStepTitles) {
		t.Errorf("onboarding step titles = %q, want %q", gotStepTitles, wantStepTitles)
	}
	if onboardingHasAttribute(steps[0], "hidden") {
		t.Error("step 1 must ship visible so a no-JS visitor can install")
	}
	for i := 1; i < len(steps); i++ {
		if !onboardingHasAttribute(steps[i], "hidden") {
			t.Errorf("step %d must ship hidden and be revealed after the prior step", i+1)
		}
	}

	// Step 3 sets honest expectations and names a model per agent.
	for _, want := range []string{"ChatGPT", "Claude Code", "Cursor", "GPT-5.6 Sol", "Opus 4.8"} {
		if !strings.Contains(onboardingRawText(document), want) {
			t.Errorf("step-3 guidance is missing %q", want)
		}
	}

	collaboration := onboardingSingleElementWithClass(t, document, "section", "collaboration")
	assertOnboardingElementText(t, collaboration, "h2", "One site, built by your team")

	// Accounts come from the company's sign-in: the page asks for no email and
	// the prompt never tells an agent to register one.
	if got := onboardingElementsByID(document, "workEmail"); len(got) != 0 {
		t.Error("homepage still asks for a work email")
	}

	installPrompt := onboardingSingleElementByID(t, document, "installPrompt")
	disclosure := onboardingAncestor(installPrompt, "details")
	if disclosure == nil {
		t.Fatal("homepage install prompt is not inside a details disclosure")
	}
	if onboardingHasAttribute(disclosure, "open") {
		t.Error("homepage install prompt disclosure is open by default")
	}
	assertOnboardingElementText(t, disclosure, "summary", "View the prompt")
	// The homepage prompt no longer carries the npx route; it installs the
	// verified bundle directly. Assert the per-agent destinations instead,
	// since those are what a first-time user actually depends on.
	for _, want := range []string{
		"<home>/.claude/skills",
		"<home>/.agents/skills",
		"%USERPROFILE% only in cmd",
		"Browser platform is unknown. Detect and verify the actual operating system and shell before continuing.",
		"<selected-root>/simple-host/references/account-recovery.md",
	} {
		if !strings.Contains(onboardingRawText(installPrompt), want) {
			t.Errorf("homepage install prompt is missing %q", want)
		}
	}

	websiteBrief := onboardingSingleElementByID(t, document, "websiteBrief")
	if websiteBrief.Data != "textarea" || !onboardingHasAttribute(websiteBrief, "required") {
		t.Error("website brief must be a required textarea")
	}
	agentPromptText := onboardingSingleElementByID(t, document, "agentPromptText")
	if agentPromptText.Data != "textarea" || !onboardingHasAttribute(agentPromptText, "readonly") {
		t.Error("generated agent prompt must be a readonly textarea")
	}
	// The prompt is copied to the clipboard, not shown; it lives inside a closed
	// disclosure within the result block, and that block starts hidden.
	if onboardingAncestor(agentPromptText, "details") == nil {
		t.Error("generated agent prompt must sit inside a disclosure, not be shown by default")
	}
	if result := onboardingSingleElementByID(t, document, "agentPromptResult"); !onboardingHasAttribute(result, "hidden") {
		t.Error("agent prompt result block must start hidden")
	}

	clientScript := onboardingScriptContaining(t, document, "function deploymentPrompt(")
	for _, want := range []string{
		"function classifyInstallPlatform(browserNavigator)",
		"browserNavigator.userAgentData.platform",
		".replace(INSTALL_PLATFORM_HINTS.unknown, classifyInstallPlatform(window.navigator))",
		"copyText(target === installPrompt ? target.textContent : target.textContent.trim())",
	} {
		if !strings.Contains(clientScript, want) {
			t.Errorf("homepage install prompt machinery is missing %q", want)
		}
	}
	if strings.Contains(homepage, "{{PLATFORM_HINT}}") {
		t.Error("homepage raw HTML still contains an unresolved platform-hint token")
	}
	emailScript := onboardingSubstringFrom(t, clientScript, "function deploymentPrompt(")
	for _, want := range []string{
		"make sure you can publish as me",
		"agentPromptText.value = deploymentPrompt",
		"localBuildRequest(brief)",
		// The owner is settled before the build. See SKILL.md section 3.
		"Do this first so the site",
		// The generated prompt must gate on the skills before any build work,
		// and must refuse the workarounds agents reach for when the skills are
		// absent — a downloadable archive, code pasted into chat, a different
		// host, or the host app's own sites feature. Building first and
		// surfacing the problem at publish time strands the user with a
		// finished site and nowhere to put it.
		"Before you build anything, check that all three Simple Host skills are installed",
		"installing them is your first task",
		"Do not build the website first",
		"Do not offer me a workaround",
		"the installed simple-host account-recovery workflow",
		"copyText(agentPromptText.value)",
		"agentPromptText.select()",
	} {
		if !strings.Contains(emailScript, want) {
			t.Errorf("work-email client code is missing %q", want)
		}
	}
	for _, stale := range []string{"save the returned API key the normal way", "register with that email", "work email"} {
		if strings.Contains(emailScript, stale) {
			t.Errorf("generated deployment prompt still says %q", stale)
		}
	}
	// Local persistence is now permitted; transmission and URL/cookie exposure
	// are not. The email may be written to localStorage but must never reach the
	// network, a cookie, the URL, or analytics. This is the whole privacy
	// contract now that "not persisted" is no longer part of it.
	for _, forbidden := range []string{
		"fetch(", "xmlhttprequest", "sendbeacon", "formdata", "sessionstorage",
		"indexeddb", "document.cookie", "urlsearchparams", "location.search", "location.href",
		"history.pushstate", "history.replacestate", "analytics", "new image(", ".src =",
	} {
		if strings.Contains(strings.ToLower(emailScript), forbidden) {
			t.Errorf("work-email client code contains forbidden transmission %q", forbidden)
		}
	}
	// The only persistence sink allowed is localStorage under the documented key,
	// and it must carry an expiry so the brief is not kept indefinitely.
	if !strings.Contains(clientScript, "STORE_TTL_MS") {
		t.Error("persisted onboarding data must expire; no TTL found")
	}

	visibleDOM := strings.ToLower(onboardingVisibleText(document))
	for _, promotion := range []string{
		"ai for deployed sites", "ai forwarder", "/api/pfb/", "x-pfb-token", "openai chat", "voice transcription",
	} {
		if strings.Contains(visibleDOM, promotion) {
			t.Errorf("visible homepage DOM promotes AI forwarders with %q", promotion)
		}
	}
}

func TestHomepageInstallPlatformClassifierBehavior(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is required for the homepage platform classifier behavior test")
	}

	homepage := readOnboardingStaticFile(t, "index.html")
	startMarker := "// PLATFORM_CLASSIFIER_START"
	endMarker := "// PLATFORM_CLASSIFIER_END"
	start := strings.Index(homepage, startMarker)
	end := strings.Index(homepage, endMarker)
	if start < 0 || end < 0 || start >= end {
		t.Fatal("homepage platform classifier markers are missing or out of order")
	}
	classifier := homepage[start+len(startMarker) : end]
	command := exec.Command(node, "testdata/homepage_platform_classifier_test.js")
	command.Env = append(os.Environ(), "SIMPLE_HOST_PLATFORM_CLASSIFIER="+classifier)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("run homepage platform classifier behavior test: %v\n%s", err, output)
	}
}

// Install comes first in the journey, and the page offers a read-only check
// prompt for anyone unsure — without claiming to verify anything, which it
// cannot. The step ordering, not a warning label, is what stops someone
// building before the skills exist.
func TestHomepageStatesInstallPrerequisiteWithoutClaimingToVerify(t *testing.T) {
	t.Parallel()

	homepage := readOnboardingStaticFile(t, "index.html")
	document := parseOnboardingHTML(t, homepage)

	// Install is step 1 and ships visible; the describe step is later and ships
	// hidden until the user says they installed. The page does not verify it —
	// see the reveal test — it just orders the work so install comes first.
	step1 := onboardingSingleElementByID(t, document, "journeyStep1")
	if onboardingHasAttribute(step1, "hidden") {
		t.Error("install step must be visible on load")
	}
	if h3 := onboardingElements(step1, "h3"); len(h3) == 0 || onboardingText(h3[0]) != "Install Simple Host" {
		t.Error("step 1 is not the install step")
	}

	check := onboardingSingleElementByID(t, document, "installCheckPrompt")
	if onboardingAncestor(check, "details") == nil {
		t.Error("check prompt is not inside a disclosure; it should stay out of the default reading path")
	}
	for _, want := range []string{
		"simple-host-builder",
		"fix-paths-for-subpath-hosting",
		"Answer yes or no",
		"Do not install or change anything.",
	} {
		if !strings.Contains(onboardingRawText(check), want) {
			t.Errorf("install check prompt is missing %q", want)
		}
	}

	// Without script the inline link cannot open the disclosure, so it hides
	// itself and the disclosure carries the affordance on its own.
	// Support routing must survive in the prompt's dead ends, not only on the
	// page: someone who pasted the prompt into their agent is no longer reading
	// this site when they get stuck. The package names no chat channel of its
	// own; the installation's platform team is where help comes from.
	prompt := onboardingRawText(onboardingSingleElementByID(t, document, "installPrompt"))
	if got := strings.Count(prompt, "platform team"); got < 2 {
		t.Errorf("install prompt names the platform team %d times, want it at every dead end", got)
	}
	if !strings.Contains(onboardingVisibleText(document), "platform team") {
		t.Error("homepage never names where to get help outside the prompt")
	}

	// The "Not sure if it's installed?" affordance is a single disclosure, not
	// also an inline link that duplicates its own summary text.
	if got := strings.Count(onboardingVisibleText(document), "Not sure if it's installed?"); got != 1 {
		t.Errorf("\"Not sure if it's installed?\" appears %d times, want exactly one disclosure", got)
	}
}

// The step reveal, persistence, and backend detection are behaviour, so this
// asserts the wiring exists rather than re-deriving it. The full click-through
// is exercised against the served page with jsdom during implementation; here
// we guard the contract that a later edit could silently break.
func TestHomepageProgressiveRevealWiring(t *testing.T) {
	t.Parallel()

	homepage := readOnboardingStaticFile(t, "index.html")
	document := parseOnboardingHTML(t, homepage)
	script := onboardingScriptContaining(t, document, "var STORE_KEY =")

	for _, want := range []string{
		// reveal is driven by explicit user assertion, one step at a time
		"data-advance-to",
		"function advanceTo(",
		// the copy button stays "Copied" because the page cannot see a paste
		"data-persist-copied",
		"button.classList.add('is-copied')",
		// progress persists locally with an expiry, and is restored on load
		"STORE_TTL_MS",
		"function restoreProgress(",
		// backend detection adds the state instruction and its warning
		"function describedSiteNeedsState(",
		"versioned shared state",
		"never as HTML",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("progressive-reveal wiring is missing %q", want)
		}
	}

	// The hidden steps must be genuinely hidden: the li carries display:grid,
	// which [hidden] alone cannot beat, so an explicit rule is required.
	if !strings.Contains(homepage, ".journey__steps > li[hidden] { display: none; }") {
		t.Error("hidden journey steps would still render: li[hidden] rule missing")
	}
}

func TestHomepagePresentationContract(t *testing.T) {
	t.Parallel()

	homepage := readOnboardingStaticFile(t, "index.html")
	document := parseOnboardingHTML(t, homepage)

	heroHeading := onboardingCSSRule(t, homepage, `\.home-hero h1`)
	assertOnboardingCSSContains(t, heroHeading,
		"font-family: var(--font-sans)",
		"font-size: clamp(42px, 6vw, 56px)",
	)

	majorHeadings := onboardingCSSRule(t, homepage, `\.home-section-heading h2,\s*\.collaboration h2`)
	assertOnboardingCSSContains(t, majorHeadings,
		"font-family: var(--font-sans)",
		"font-size: clamp(34px, 5vw, 42px)",
	)

	hero := onboardingCSSRule(t, homepage, `\.home-hero`)
	for _, forbidden := range []string{"min-height", "gradient"} {
		if strings.Contains(strings.ToLower(hero), forbidden) {
			t.Errorf(".home-hero CSS contains forbidden %q: %s", forbidden, hero)
		}
	}
	if regexp.MustCompile(`(?s)\.home-hero::after\s*\{`).MatchString(homepage) {
		t.Error("homepage defines a .home-hero::after decoration")
	}
	heroContent := onboardingCSSRule(t, homepage, `\.home-hero__content`)
	if strings.Contains(strings.ToLower(heroContent), "animation") {
		t.Errorf(".home-hero__content CSS contains an entrance animation: %s", heroContent)
	}

	sectionIntroduction := onboardingCSSRule(t, homepage, `\.home-section-heading`)
	assertOnboardingCSSContains(t, sectionIntroduction,
		"max-width: 680px",
		"margin: 0 auto 52px",
		"text-align: center",
	)
	assertOnboardingCSSContains(t, onboardingCSSRule(t, homepage, `\.journey`), "margin: 0 auto")
	assertOnboardingCSSContains(t, onboardingCSSRule(t, homepage, `\.home-faq`), "margin: 0 auto")
	collaborationIntroduction := onboardingCSSRule(t, homepage, `\.collaboration__copy`)
	assertOnboardingCSSContains(t, collaborationIntroduction,
		"max-width: 680px",
		"margin: 0 auto",
		"text-align: center",
	)

	collaboration := onboardingCSSRule(t, homepage, `\.collaboration`)
	assertOnboardingCSSContains(t, collaboration,
		"background: #f7faff",
		"color: var(--ink)",
	)
	assertOnboardingCSSContains(t, onboardingCSSRule(t, homepage, `\.collaboration h2`), "color: var(--ink)")
	assertOnboardingCSSContains(t, onboardingCSSRule(t, homepage, `\.collaboration__copy > p:not\(\.home-eyebrow\)`), "color: var(--ink-soft)")
	assertOnboardingCSSContains(t, onboardingCSSRule(t, homepage, `\.collaboration__flow li`), "color: var(--ink)")

	if len(onboardingElementsWithClass(document, "", "final-cta")) != 0 {
		t.Error("homepage contains final-cta markup")
	}
	if strings.Contains(homepage, ".final-cta") {
		t.Error("homepage contains final-cta CSS")
	}
}

func TestCapabilitiesUsesPlainProductLanguage(t *testing.T) {
	t.Parallel()

	page := readOnboardingStaticFile(t, "capabilities.html")
	document := parseOnboardingHTML(t, page)
	assertOnboardingElementText(t, document, "h1", "What Simple Host does")

	var headings []string
	for _, section := range onboardingElements(document, "section") {
		for _, heading := range onboardingDirectChildren(section, "h2") {
			headings = append(headings, onboardingText(heading))
		}
	}
	wantHeadings := []string{
		"Publish a website",
		"Update it or go back",
		"Work on one site together",
		"Save shared information",
		"Choose who can open it",
		"Check activity",
		"Good to know",
	}
	if !slices.Equal(headings, wantHeadings) {
		t.Errorf("capability headings = %q, want %q", headings, wantHeadings)
	}

	if len(onboardingElements(document, "pre")) != 0 || len(onboardingElements(document, "code")) != 0 {
		t.Error("Capabilities exposes implementation code instead of product guidance")
	}
	visible := strings.ToLower(onboardingVisibleText(document))
	for _, forbidden := range []string{"anthropic-shaped", "openai chat completions", "voice transcription", "json state", "/api/pfb/"} {
		if strings.Contains(visible, forbidden) {
			t.Errorf("Capabilities still exposes technical promotion %q", forbidden)
		}
	}
	if strings.Contains(page, "var(--font-display)") || strings.Contains(page, "class=\"display\"") {
		t.Error("Capabilities still uses display typography")
	}
}

func TestInstallRecommendedAgentPromptContract(t *testing.T) {
	t.Parallel()

	installPage := readOnboardingStaticFile(t, "install.html")
	document := parseOnboardingHTML(t, installPage)
	recommended := onboardingSingleElementWithClass(t, document, "div", "recommended")

	// The primary control opens the reader's own agent with the prompt already
	// typed in. Copying is recovery, not the main road: script moves that button
	// into the fallback line, so it must still exist with the same identity.
	buttons := onboardingElements(recommended, "button")
	agents := map[string]bool{}
	copyControls := 0
	for _, button := range buttons {
		if agent, ok := onboardingAttribute(button, "data-agent"); ok {
			agents[agent] = true
			continue
		}
		id, _ := onboardingAttribute(button, "id")
		if id != "copyInstallPrompt" || onboardingText(button) != "Copy install prompt" {
			t.Errorf("unexpected control in recommended install: id %q text %q", id, onboardingText(button))
			continue
		}
		copyControls++
	}
	if copyControls != 1 {
		t.Errorf("recommended agent install has %d copy controls, want exactly one", copyControls)
	}
	for _, agent := range []string{"chatgpt", "claude", "cursor"} {
		if !agents[agent] {
			t.Errorf("agent launcher is missing a tab for %q", agent)
		}
	}
	if len(agents) != 3 {
		t.Errorf("agent launcher offers %d agents, want exactly three", len(agents))
	}

	promptNode := onboardingSingleElementByID(t, document, "installPrompt")
	promptDisclosure := onboardingAncestor(promptNode, "details")
	if promptDisclosure == nil {
		t.Fatal("agent install prompt is not inside a details disclosure")
	}
	if onboardingHasAttribute(promptDisclosure, "open") {
		t.Error("agent install prompt is visible by default")
	}
	assertOnboardingElementText(t, promptDisclosure, "summary", "View full agent prompt")

	clientScript := onboardingScriptContaining(t, document, "var installPrompt =")
	if !strings.Contains(clientScript, "copyText(installPrompt.textContent)") {
		t.Error("primary install control does not copy the hidden agent prompt")
	}
	for _, homepageOnly := range []string{"{{PLATFORM_HINT}}", "classifyInstallPlatform", "Browser hint:"} {
		if strings.Contains(installPage, homepageOnly) {
			t.Errorf("platform-neutral Install page contains homepage-only marker %q", homepageOnly)
		}
	}

	// The prompt is for a first-time, non-technical user with zero installs.
	// It is deliberately short: the version it replaced had grown to 6,722
	// characters of branching for cases this audience never hits. Guard the
	// order of the steps that must not be reordered, and the facts that were
	// wrong before: a normal chat window cannot do this, the destination
	// differs per agent, and %USERPROFILE% is cmd-only.
	prompt := onboardingRawText(promptNode)
	assertOnboardingSubstringsInOrder(t, prompt,
		"First check whether you can actually read and write files on my computer.",
		"Install into exactly one folder:",
		"Read {{SIMPLE_HOST_ORIGIN}}/skills/version as JSON",
		"From the exact skills root you selected, directly read <selected-root>/simple-host/references/account-recovery.md completely.",
		"Before changing anything, tell me conversationally in one or two sentences",
	)
	for _, want := range []string{
		// Capability, not menu navigation: the app's mode names changed in the
		// ChatGPT/Codex merger and would go stale in the prompt.
		"an ordinary ChatGPT chat window cannot",
		// Verified per-vendor roots. Claude Code does not read ~/.agents/skills;
		// ChatGPT and Cursor both do.
		"<home>/.claude/skills",
		"<home>/.agents/skills",
		// %USERPROFILE% does not expand in PowerShell and can create a folder
		// with that literal name.
		"%USERPROFILE% only in cmd",
		// Expand-Archive otherwise nests the bundle inside an extra folder and
		// the skills are never discovered.
		"with no extra folder wrapped around them",
		"Do not run anything out of the archive.",
		"none still says {{VERSION}}",
		// Registration is deliberately versionless. The directly read reference
		// owns registration and persistence before skill discovery is available.
		"Do not rely on the newly copied skill being discoverable yet.",
		"exact-destination preflight",
		"save-and-verify steps",
		"one yes covering both",
		"honor every approval required by the host, tool, sandbox, or operating system",
		// A successful install looks broken without this.
		"a new chat or an app restart",
		"ask my platform team",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("agent install prompt is missing %q", want)
		}
	}

	// Machinery removed on purpose. If any of this comes back, the prompt is
	// drifting toward the unusable version again.
	for _, gone := range []string{
		"Enumerate every skills root",
		"npx",
		"permissioned updater",
		"Before approval:",
		"#simple-host-support",
		"work email",
	} {
		if strings.Contains(prompt, gone) {
			t.Errorf("agent install prompt reintroduced %q; it is not relevant to a first install", gone)
		}
	}

	if len(prompt) > 2600 {
		t.Errorf("agent install prompt is %d characters; it must stay short enough for a non-technical user to paste", len(prompt))
	}
}

func TestInstallNpxCommandsArePinnedExplicitAndSameOrigin(t *testing.T) {
	t.Parallel()

	installPage := readOnboardingStaticFile(t, "install.html")
	document := parseOnboardingHTML(t, installPage)
	commands := onboardingNpxCommandLines(document)
	if len(commands) < 4 {
		t.Fatalf("found %d displayed npx commands, want at least 4", len(commands))
	}

	versions := make(map[string]bool)
	versionPattern := regexp.MustCompile(`skills@\d+\.\d+\.\d+`)
	baseOriginSource := regexp.MustCompile(`\badd\s+"?\{\{SIMPLE_HOST_ORIGIN\}\}"?\s+--global\b`)
	for i, command := range commands {
		for _, version := range versionPattern.FindAllString(command, -1) {
			versions[version] = true
		}
		if !regexp.MustCompile(`\bnpx(?:\.cmd)? --yes skills@1\.5\.20 add`).MatchString(command) {
			t.Errorf("npx command %d does not pin skills@1.5.20 with npm --yes: %s", i, command)
		}
		if !strings.HasSuffix(command, "--yes") || strings.Count(command, "--yes") != 2 {
			t.Errorf("npx command %d does not have both npm and trailing CLI --yes: %s", i, command)
		}
		if !strings.Contains(command, "--global") || !strings.Contains(command, "--copy") {
			t.Errorf("npx command %d does not use global copy mode: %s", i, command)
		}
		if strings.Count(command, "--agent ") != 1 {
			t.Errorf("npx command %d has %d --agent flags, want 1: %s", i, strings.Count(command, "--agent "), command)
		}
		if strings.Count(command, "--skill ") != 3 {
			t.Errorf("npx command %d has %d --skill flags, want 3: %s", i, strings.Count(command, "--skill "), command)
		}
		var selectedSkills []string
		fields := strings.Fields(command)
		for fieldIndex, field := range fields {
			if field == "--skill" && fieldIndex+1 < len(fields) {
				selectedSkills = append(selectedSkills, fields[fieldIndex+1])
			}
		}
		wantSkills := []string{"simple-host", "simple-host-builder", "fix-paths-for-subpath-hosting"}
		if !slices.Equal(selectedSkills, wantSkills) {
			t.Errorf("npx command %d selects skills %q, want %q: %s", i, selectedSkills, wantSkills, command)
		}
		if !strings.Contains(command, "DISABLE_TELEMETRY=1") &&
			!strings.Contains(command, "DISABLE_TELEMETRY='1'") &&
			!strings.Contains(command, `DISABLE_TELEMETRY="1"`) {
			t.Errorf("npx command %d does not disable telemetry: %s", i, command)
		}
		if !baseOriginSource.MatchString(command) {
			t.Errorf("npx command %d must pass the base Simple Host origin as its source: %s", i, command)
		}
		if strings.Contains(command, "/.well-known/agent-skills/index.json") {
			t.Errorf("npx command %d passes the discovery index directly instead of the base origin: %s", i, command)
		}
	}
	if len(versions) != 1 || !versions["skills@1.5.20"] {
		t.Errorf("displayed npx versions = %v, want only skills@1.5.20", versions)
	}
}

func TestInstallManualFallbackIsLocalVerifiedAndCrossPlatform(t *testing.T) {
	t.Parallel()

	installPage := readOnboardingStaticFile(t, "install.html")
	document := parseOnboardingHTML(t, installPage)
	manual := onboardingSingleElementByID(t, document, "manual-install")
	manualLists := onboardingElementsWithClass(manual, "ol", "manual-steps")
	if len(manualLists) != 1 {
		t.Fatalf("manual install has %d numbered lists, want 1", len(manualLists))
	}
	steps := onboardingDirectChildren(manualLists[0], "li")
	if len(steps) != 3 {
		t.Fatalf("manual install has %d direct numbered steps, want exactly 3", len(steps))
	}

	wantStepStarts := []string{
		"Download the ZIP and manifest.",
		"Verify, then extract into one skills root.",
		"Restart and list the skills.",
	}
	for i, want := range wantStepStarts {
		if !strings.HasPrefix(onboardingText(steps[i]), want) {
			t.Errorf("manual step %d = %q, want prefix %q", i+1, onboardingText(steps[i]), want)
		}
	}

	verificationScript := onboardingScriptContaining(t, document, "zipInput.addEventListener('change'")
	for _, want := range []string{
		"fetch('/skills/version'",
		"file.size !== manifest.bundle_size",
		"window.crypto.subtle.digest('SHA-256', contents)",
		"var generation = ++zipVerificationGeneration",
		"generation === zipVerificationGeneration",
		"zipInput.files[0] === file",
	} {
		if !strings.Contains(verificationScript, want) {
			t.Errorf("manual ZIP verification is missing %q", want)
		}
	}
	for _, forbidden := range []string{
		`(?i)\bFormData\b`,
		`(?i)\b(?:localStorage|sessionStorage|indexedDB)\b`,
		`(?i)\.upload\b`,
		`(?i)method\s*:\s*['"](?:POST|PUT)['"]`,
	} {
		if regexp.MustCompile(forbidden).MatchString(verificationScript) {
			t.Errorf("manual ZIP verification contains forbidden operation matching %q", forbidden)
		}
	}

	pageText := onboardingText(document)
	for _, want := range []string{
		"The selected file stays in this browser tab. It is not uploaded, stored, or transmitted; only the manifest is fetched from this origin.",
		"It is an integrity check, not a code-signing signature or independent proof of publisher identity.",
		"~/.agents/skills",
		`$HOME\.agents\skills`,
		"~/.claude/skills",
		`$HOME\.claude\skills`,
		"Required verification: after any command, confirm the selected root contains simple-host/SKILL.md",
		"Treat any partial result as a failed install",
		"remove only skill roots newly created by that attempt",
	} {
		if !strings.Contains(pageText, want) {
			t.Errorf("manual install page is missing %q", want)
		}
	}

	commands := onboardingNpxCommandLines(document)
	var foundPOSIX, foundWindows bool
	for _, command := range commands {
		foundPOSIX = foundPOSIX || strings.HasPrefix(command, "DISABLE_TELEMETRY=1 npx")
		foundWindows = foundWindows ||
			(strings.HasPrefix(command, "$env:DISABLE_TELEMETRY=") && strings.Contains(command, "; npx.cmd --yes"))
	}
	if !foundPOSIX || !foundWindows {
		t.Errorf("platform commands found: POSIX=%t Windows=%t, want both", foundPOSIX, foundWindows)
	}
}

func TestChangelogStartsEmptyForANewInstallation(t *testing.T) {
	t.Parallel()

	changelog := readOnboardingStaticFile(t, "changelog.html")
	for _, want := range []string{
		"No releases yet",
		"This installation is new",
	} {
		if !strings.Contains(changelog, want) {
			t.Errorf("changelog is missing %q", want)
		}
	}
	for _, stale := range []string{"PFB", "Okta sign-in", "v0."} {
		if strings.Contains(changelog, stale) {
			t.Errorf("changelog still carries release history: %q", stale)
		}
	}
}

func readOnboardingStaticFile(t *testing.T, name string) string {
	t.Helper()
	body, err := fs.ReadFile(staticFiles, "static/"+name)
	if err != nil {
		t.Fatalf("read embedded static/%s: %v", name, err)
	}
	return string(body)
}

func onboardingCSSRule(t *testing.T, document, selectorPattern string) string {
	t.Helper()
	pattern := regexp.MustCompile(`(?s)` + selectorPattern + `\s*\{([^{}]*)\}`)
	match := pattern.FindStringSubmatch(document)
	if len(match) != 2 {
		t.Fatalf("stylesheet has no rule matching %q", selectorPattern)
	}
	return match[1]
}

func assertOnboardingCSSContains(t *testing.T, rule string, declarations ...string) {
	t.Helper()
	for _, declaration := range declarations {
		if !strings.Contains(rule, declaration) {
			t.Errorf("CSS rule is missing %q: %s", declaration, rule)
		}
	}
}

func renderOnboardingShowcase(t *testing.T) string {
	t.Helper()
	response := httptest.NewRecorder()
	newTestShowcaseHandler(openShowcaseTestDB(t, showcaseGalleryTestScript()), testHostModel(t)).page(response, httptest.NewRequest(http.MethodGet, "/showcase", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("render Showcase: status = %d, want 200; body=%s", response.Code, response.Body.String())
	}
	return response.Body.String()
}

func parseOnboardingHTML(t *testing.T, document string) *htmlnode.Node {
	t.Helper()
	root, err := htmlnode.Parse(strings.NewReader(document))
	if err != nil {
		t.Fatalf("parse HTML: %v", err)
	}
	return root
}

func onboardingElements(root *htmlnode.Node, tag string) []*htmlnode.Node {
	var result []*htmlnode.Node
	var visit func(*htmlnode.Node)
	visit = func(node *htmlnode.Node) {
		if node.Type == htmlnode.ElementNode && (tag == "" || node.Data == tag) {
			result = append(result, node)
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			visit(child)
		}
	}
	visit(root)
	return result
}

func onboardingElementsWithAttribute(root *htmlnode.Node, name string) []*htmlnode.Node {
	var result []*htmlnode.Node
	for _, element := range onboardingElements(root, "") {
		if onboardingHasAttribute(element, name) {
			result = append(result, element)
		}
	}
	return result
}

func onboardingElementsByID(root *htmlnode.Node, id string) []*htmlnode.Node {
	var result []*htmlnode.Node
	for _, element := range onboardingElements(root, "") {
		if value, _ := onboardingAttribute(element, "id"); value == id {
			result = append(result, element)
		}
	}
	return result
}

func onboardingElementsWithClass(root *htmlnode.Node, tag, class string) []*htmlnode.Node {
	var result []*htmlnode.Node
	for _, element := range onboardingElements(root, tag) {
		if onboardingHasClass(element, class) {
			result = append(result, element)
		}
	}
	return result
}

func onboardingSingleElementByID(t *testing.T, root *htmlnode.Node, id string) *htmlnode.Node {
	t.Helper()
	elements := onboardingElementsByID(root, id)
	if len(elements) != 1 {
		t.Fatalf("found %d elements with id %q, want 1", len(elements), id)
	}
	return elements[0]
}

func onboardingSingleElementWithClass(t *testing.T, root *htmlnode.Node, tag, class string) *htmlnode.Node {
	t.Helper()
	elements := onboardingElementsWithClass(root, tag, class)
	if len(elements) != 1 {
		t.Fatalf("found %d <%s> elements with class %q, want 1", len(elements), tag, class)
	}
	return elements[0]
}

func onboardingDirectChildren(root *htmlnode.Node, tag string) []*htmlnode.Node {
	var result []*htmlnode.Node
	for child := root.FirstChild; child != nil; child = child.NextSibling {
		if child.Type == htmlnode.ElementNode && child.Data == tag {
			result = append(result, child)
		}
	}
	return result
}

func onboardingAncestor(node *htmlnode.Node, tag string) *htmlnode.Node {
	for parent := node.Parent; parent != nil; parent = parent.Parent {
		if parent.Type == htmlnode.ElementNode && parent.Data == tag {
			return parent
		}
	}
	return nil
}

func onboardingAttribute(node *htmlnode.Node, name string) (string, bool) {
	for _, attribute := range node.Attr {
		if attribute.Key == name {
			return attribute.Val, true
		}
	}
	return "", false
}

func onboardingHasAttribute(node *htmlnode.Node, name string) bool {
	_, ok := onboardingAttribute(node, name)
	return ok
}

func onboardingHasClass(node *htmlnode.Node, class string) bool {
	classes, _ := onboardingAttribute(node, "class")
	return slices.Contains(strings.Fields(classes), class)
}

func onboardingHasElementWithAttribute(root *htmlnode.Node, tag, name, value string) bool {
	for _, element := range onboardingElements(root, tag) {
		if got, _ := onboardingAttribute(element, name); got == value {
			return true
		}
	}
	return false
}

func onboardingRawText(root *htmlnode.Node) string {
	var text strings.Builder
	var visit func(*htmlnode.Node)
	visit = func(node *htmlnode.Node) {
		if node.Type == htmlnode.TextNode {
			text.WriteString(node.Data)
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			visit(child)
		}
	}
	visit(root)
	return text.String()
}

func onboardingText(root *htmlnode.Node) string {
	return strings.Join(strings.Fields(onboardingRawText(root)), " ")
}

func onboardingTextOrEmpty(nodes []*htmlnode.Node) string {
	if len(nodes) == 0 {
		return ""
	}
	return onboardingText(nodes[0])
}

func onboardingVisibleText(root *htmlnode.Node) string {
	var text strings.Builder
	var visit func(*htmlnode.Node, bool)
	visit = func(node *htmlnode.Node, hidden bool) {
		if node.Type == htmlnode.ElementNode {
			hidden = hidden || node.Data == "script" || node.Data == "style" || node.Data == "template" || onboardingHasAttribute(node, "hidden")
		}
		if node.Type == htmlnode.TextNode && !hidden {
			text.WriteByte(' ')
			text.WriteString(node.Data)
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			visit(child, hidden)
		}
	}
	visit(root, false)
	return strings.Join(strings.Fields(text.String()), " ")
}

func onboardingScriptContaining(t *testing.T, document *htmlnode.Node, marker string) string {
	t.Helper()
	for _, script := range onboardingElements(document, "script") {
		content := onboardingRawText(script)
		if strings.Contains(content, marker) {
			return content
		}
	}
	t.Fatalf("no script contains %q", marker)
	return ""
}

func onboardingSubstringBetween(t *testing.T, value, start, end string) string {
	t.Helper()
	startIndex := strings.Index(value, start)
	if startIndex < 0 {
		t.Fatalf("text does not contain start marker %q", start)
	}
	endIndex := strings.Index(value[startIndex+len(start):], end)
	if endIndex < 0 {
		t.Fatalf("text after %q does not contain end marker %q", start, end)
	}
	return value[startIndex : startIndex+len(start)+endIndex]
}

func onboardingSubstringFrom(t *testing.T, value, start string) string {
	t.Helper()
	startIndex := strings.Index(value, start)
	if startIndex < 0 {
		t.Fatalf("text does not contain start marker %q", start)
	}
	return value[startIndex:]
}

func onboardingNpxCommandLines(document *htmlnode.Node) []string {
	var commands []string
	for _, code := range onboardingElements(document, "code") {
		for _, line := range strings.Split(onboardingRawText(code), "\n") {
			line = strings.Join(strings.Fields(line), " ")
			if strings.Contains(line, "npx --yes skills@") || strings.Contains(line, "npx.cmd --yes skills@") {
				commands = append(commands, line)
			}
		}
	}
	return commands
}

func assertOnboardingElementText(t *testing.T, root *htmlnode.Node, tag, want string) {
	t.Helper()
	elements := onboardingElements(root, tag)
	if len(elements) == 0 {
		t.Fatalf("found no <%s> below <%s>", tag, root.Data)
	}
	if got := onboardingText(elements[0]); got != want {
		t.Errorf("first <%s> text = %q, want %q", tag, got, want)
	}
}

func assertOnboardingAttributes(t *testing.T, node *htmlnode.Node, want map[string]string) {
	t.Helper()
	for name, wantValue := range want {
		if got, _ := onboardingAttribute(node, name); got != wantValue {
			t.Errorf("#%s %s = %q, want %q", attributeOr(node, "id", node.Data), name, got, wantValue)
		}
	}
}

func assertOnboardingSubstringsInOrder(t *testing.T, value string, fragments ...string) {
	t.Helper()
	position := 0
	for _, fragment := range fragments {
		index := strings.Index(value[position:], fragment)
		if index < 0 {
			t.Fatalf("text after byte %d does not contain ordered fragment %q", position, fragment)
		}
		position += index + len(fragment)
	}
}

func attributeOr(node *htmlnode.Node, name, fallback string) string {
	if value, ok := onboardingAttribute(node, name); ok {
		return value
	}
	return fallback
}
