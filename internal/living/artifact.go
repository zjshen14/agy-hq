package living

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/store"
)

// Build compiles `<vault>/living_spaces/<slug>/space.json` into `<vault>/living_spaces/<slug>/index.html` (0600)
// in 0 LLM tokens and returns the rendered HTML path and byte count.
func Build(vaultDir, slug string) (string, int, error) {
	doc, err := Load(vaultDir, slug)
	if err != nil {
		return "", 0, err
	}
	htmlBytes, err := RenderHTML(doc, RenderOptions{Surface: "webui"})
	if err != nil {
		return "", 0, err
	}
	outPath := spaceHTMLPath(vaultDir, doc.Slug)
	if err := store.With(context.Background(), store.RankLivingSpaces, spaceLockPath(vaultDir, doc.Slug), func(_ context.Context) error {
		return store.WriteAtomic(outPath, htmlBytes, 0o600)
	}); err != nil {
		return "", 0, err
	}
	return outPath, len(htmlBytes), nil
}

// ExportArtifact compiles the Living Space as a self-contained Antigravity `generative_ui` HTML artifact
// inside `artifactDir` (`space_<slug>.html` + `.metadata.json`) so it renders natively on both desktop
// and Antigravity Remote Control mobile/web without needing localhost `:4242`.
func ExportArtifact(vaultDir, slug, artifactDir string, now time.Time) (string, error) {
	if strings.TrimSpace(artifactDir) == "" {
		return "", fmt.Errorf("artifactDir is required")
	}
	if now.IsZero() {
		now = time.Now()
	}
	doc, err := Load(vaultDir, slug)
	if err != nil {
		return "", err
	}
	htmlBytes, err := RenderHTML(doc, RenderOptions{Surface: "artifact"})
	if err != nil {
		return "", err
	}
	htmlPath := filepath.Join(artifactDir, "space_"+doc.Slug+".html")
	if err := store.WriteAtomic(htmlPath, htmlBytes, 0o600); err != nil {
		return "", err
	}
	primary := "en"
	if len(doc.Locales) > 0 {
		primary = doc.Locales[0]
	}
	meta := map[string]interface{}{
		"artifactType": "ARTIFACT_TYPE_OTHER",
		"summary":      fmt.Sprintf("Living Space: %s (%s)", doc.Title.Resolve(primary, "en"), doc.Slug),
		"updatedAt":    now.UTC().Format(time.RFC3339Nano),
		"userFacing":   true,
	}
	metaBytes, _ := json.MarshalIndent(meta, "", "  ")
	_ = store.WriteAtomic(htmlPath+".metadata.json", append(metaBytes, '\n'), 0o600)
	return htmlPath, nil
}

// ExportStandalone writes a portable standalone HTML copy of the Living Space to outPath.
func ExportStandalone(vaultDir, slug, outPath string) (string, error) {
	if strings.TrimSpace(outPath) == "" {
		return "", fmt.Errorf("output path is required")
	}
	doc, err := Load(vaultDir, slug)
	if err != nil {
		return "", err
	}
	htmlBytes, err := RenderHTML(doc, RenderOptions{Surface: "standalone"})
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(outPath), 0o700); err != nil {
		return "", err
	}
	if err := store.WriteAtomic(outPath, htmlBytes, 0o600); err != nil {
		return "", err
	}
	return outPath, nil
}
