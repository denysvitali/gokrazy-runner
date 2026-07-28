package webui

import (
	"io/fs"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The embedded UI has no build step and no test runner of its own, so these
// tests stand in for the checks a bundler would do: they catch the class of
// breakage that only shows up in a browser.

func assetSources(t *testing.T) (html, js, css string) {
	t.Helper()
	body, err := IndexHTML()
	if err != nil {
		t.Fatalf("IndexHTML: %v", err)
	}
	appJS, err := fs.ReadFile(AssetsFS(), "app.js")
	if err != nil {
		t.Fatalf("read app.js: %v", err)
	}
	style, err := fs.ReadFile(AssetsFS(), "style.css")
	if err != nil {
		t.Fatalf("read style.css: %v", err)
	}
	return string(body), string(appJS), string(style)
}

var (
	idAttrRe   = regexp.MustCompile(`id="([^"]+)"`)
	qsRe       = regexp.MustCompile(`qs\("#([a-zA-Z0-9_-]+)"\)`)
	classRe    = regexp.MustCompile(`class="([^"]+)"`)
	jsClassRe  = regexp.MustCompile(`className = "([a-zA-Z0-9_ -]+)"`)
	addClassRe = regexp.MustCompile(`classList\.add\("([a-zA-Z0-9_-]+)"\)`)
	dataTabRe  = regexp.MustCompile(`data-(?:tab|goto)="([^"]+)"`)
)

// TestPanelsHideWhenHidden guards the bug that made the tab bar look broken:
// `.panel { display: flex }` is an author rule, and author rules beat the UA
// stylesheet's `[hidden] { display: none }`. Without an explicit override
// every panel renders at once and clicking a tab appears to do nothing.
func TestPanelsHideWhenHidden(t *testing.T) {
	_, _, css := assetSources(t)

	rule := regexp.MustCompile(`\.panel\[hidden\]\s*\{[^}]*display:\s*none`)
	if !rule.MatchString(css) {
		t.Fatal("style.css must contain `.panel[hidden] { display: none }`; " +
			"without it .panel's own display overrides the hidden attribute " +
			"and every tab panel is visible at once")
	}

	// Any element the JS toggles via .hidden needs the same treatment when a
	// class also sets display on it.
	for _, sel := range []string{".panel"} {
		display := regexp.MustCompile(regexp.QuoteMeta(sel) + `\s*\{[^}]*display:\s*(flex|grid|block)`)
		if display.MatchString(css) {
			override := regexp.MustCompile(regexp.QuoteMeta(sel) + `\[hidden\]\s*\{[^}]*display:\s*none`)
			if !override.MatchString(css) {
				t.Errorf("%s sets display but has no [hidden] override", sel)
			}
		}
	}
}

// TestEveryReferencedElementExists catches a rename in index.html that would
// leave app.js calling addEventListener on null — which aborts the whole
// DOMContentLoaded handler, so one stale ID silently blanks the page.
func TestEveryReferencedElementExists(t *testing.T) {
	html, js, _ := assetSources(t)

	ids := map[string]bool{}
	for _, m := range idAttrRe.FindAllStringSubmatch(html, -1) {
		ids[m[1]] = true
	}

	var missing []string
	for _, m := range qsRe.FindAllStringSubmatch(js, -1) {
		if !ids[m[1]] {
			missing = append(missing, m[1])
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Fatalf("app.js references IDs that index.html does not define: %v", missing)
	}
}

func TestNoDuplicateElementIDs(t *testing.T) {
	html, _, _ := assetSources(t)

	seen := map[string]int{}
	for _, m := range idAttrRe.FindAllStringSubmatch(html, -1) {
		seen[m[1]]++
	}
	for id, n := range seen {
		if n > 1 {
			t.Errorf("id %q appears %d times; querySelector would only ever find the first", id, n)
		}
	}
}

// TestEveryClassHasStyling catches the other half of a rename: markup that
// still carries a class the stylesheet no longer defines renders unstyled
// rather than failing loudly.
func TestEveryClassHasStyling(t *testing.T) {
	html, js, css := assetSources(t)

	used := map[string]bool{}
	collect := func(matches [][]string) {
		for _, m := range matches {
			for _, name := range strings.Fields(m[1]) {
				used[name] = true
			}
		}
	}
	collect(classRe.FindAllStringSubmatch(html, -1))
	collect(jsClassRe.FindAllStringSubmatch(js, -1))
	collect(addClassRe.FindAllStringSubmatch(js, -1))

	var missing []string
	for name := range used {
		if !strings.Contains(css, "."+name) {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("classes used in markup/JS but absent from style.css: %v", missing)
	}
}

// TestTabTargetsResolve keeps the nav honest: every data-tab and data-goto
// must name a panel that exists, or the click silently does nothing.
func TestTabTargetsResolve(t *testing.T) {
	html, _, _ := assetSources(t)

	ids := map[string]bool{}
	for _, m := range idAttrRe.FindAllStringSubmatch(html, -1) {
		ids[m[1]] = true
	}

	found := 0
	for _, m := range dataTabRe.FindAllStringSubmatch(html, -1) {
		found++
		if !ids["panel-"+m[1]] {
			t.Errorf("data-tab/data-goto %q has no matching #panel-%s", m[1], m[1])
		}
	}
	if found == 0 {
		t.Fatal("no data-tab attributes found; the tab bar would be inert")
	}
}
