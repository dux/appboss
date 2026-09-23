package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"text/tabwriter"

	"dboss/internal/apps"
	"dboss/internal/config"
	"dboss/internal/pages"
)

// pageTemplate is the operand that names template.html in `dboss pages dump`.
const pageTemplate = "template"

// pagesScope is where `dboss pages` looks and writes: the app's pages folder first (none for the
// host), then the host's.
type pagesScope struct {
	app     string
	appDir  string
	hostDir string
}

func (s pagesScope) target() string {
	if s.app != "" {
		return s.appDir
	}
	return s.hostDir
}

// pages lists which file serves every dboss page, or writes the built-in ones out to edit.
func (c CLI) pages(args []string) error {
	set, pathFlag, jsonOutput := c.localFlags("pages")
	all := set.Bool("all", false, "dump the template and every page")
	force := set.Bool("force", false, "overwrite existing files")
	operands, err := parseSubcommandFlags(set, args)
	if err != nil {
		return err
	}
	dump := len(operands) > 0 && operands[0] == "dump"
	if dump {
		operands = operands[1:]
	}
	path, err := findConfig(*pathFlag)
	if err != nil {
		return err
	}
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	var app string
	if len(operands) > 0 && !isPageName(operands[0]) {
		app, operands = operands[0], operands[1:]
	}
	scope, err := resolvePagesScope(cfg, app)
	if err != nil {
		return err
	}
	if !dump {
		if len(operands) > 0 {
			return errors.New("usage: dboss pages [app] | dump [app] [name...] [--all] [--force]")
		}
		return c.listPages(scope, *jsonOutput)
	}
	return c.dumpPages(scope, operands, *all, *force)
}

func isPageName(value string) bool {
	_, ok := pages.Find(pages.Name(value))
	return ok || value == pageTemplate
}

// resolvePagesScope finds the folders: a named app under a host, the app of a single-app config,
// or the host alone.
func resolvePagesScope(cfg config.Config, app string) (pagesScope, error) {
	scope := pagesScope{hostDir: cfg.Pages}
	switch {
	case app != "":
		found, err := apps.Lookup(cfg, app)
		if err != nil {
			return scope, err
		}
		scope.app, scope.appDir = found.Name, resolvePagePath(found.Dir, found.Config.Pages)
	case cfg.App != nil:
		scope.app, scope.appDir = filepath.Base(cfg.Dir), resolvePagePath(cfg.Dir, cfg.App.Pages)
	}
	return scope, nil
}

func resolvePagePath(dir, value string) string {
	if filepath.IsAbs(value) {
		return value
	}
	return filepath.Join(dir, value)
}

// scopeSpecs are the pages a scope serves: an app never serves the host pages.
func (s pagesScope) specs() []pages.Spec {
	var result []pages.Spec
	for _, spec := range pages.Specs {
		if s.app != "" && spec.Host {
			continue
		}
		result = append(result, spec)
	}
	return result
}

type pageRow struct {
	Name   string `json:"name"`
	Status int    `json:"status"`
	File   string `json:"file"`
	When   string `json:"when"`
}

func (c CLI) listPages(scope pagesScope, jsonOutput bool) error {
	var rows []pageRow
	for _, spec := range scope.specs() {
		dirs := []string{scope.hostDir}
		if scope.app != "" {
			dirs = []string{scope.appDir, scope.hostDir}
		}
		file, _ := pages.Lookup(spec.Name, dirs...)
		if file == "" {
			file = "built-in"
		}
		rows = append(rows, pageRow{Name: string(spec.Name), Status: spec.Status, File: file, When: spec.When})
	}
	if jsonOutput {
		encoded, _ := json.MarshalIndent(rows, "", "  ")
		fmt.Fprintln(c.Out, string(encoded))
		return nil
	}
	writer := tabwriter.NewWriter(c.Out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(writer, "PAGE\tSTATUS\tFILE\tSERVED WHEN")
	for _, row := range rows {
		fmt.Fprintf(writer, "%s\t%d\t%s\t%s\n", row.Name, row.Status, row.File, row.When)
	}
	if err := writer.Flush(); err != nil {
		return err
	}
	fmt.Fprintf(c.Out, "\nfolder: %s\n", scope.target())
	fmt.Fprintln(c.Out, "a page is <name>.html in the folder, else template.html, else the host's, else built in")
	return nil
}

// dumpPages writes the built-in template, or pages with their wording written in, to the scope's
// folder. An existing file is kept unless force is set.
func (c CLI) dumpPages(scope pagesScope, names []string, all, force bool) error {
	if all {
		names = []string{pageTemplate}
		for _, spec := range scope.specs() {
			names = append(names, string(spec.Name))
		}
	}
	if len(names) == 0 {
		names = []string{pageTemplate}
	}
	for _, name := range names {
		if !isPageName(name) {
			return fmt.Errorf("unknown page %q", name)
		}
		if spec, _ := pages.Find(pages.Name(name)); spec.Host && scope.app != "" {
			return fmt.Errorf("%s is a host page; run dboss pages dump %s without an app", name, name)
		}
	}
	dir := scope.target()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	skipped := 0
	for _, name := range slices.Compact(names) {
		file, data := pages.TemplateFile, pages.Template
		if name != pageTemplate {
			file, data = name+".html", pages.Source(pages.Name(name))
		}
		target := filepath.Join(dir, file)
		if _, err := os.Stat(target); err == nil && !force {
			fmt.Fprintf(c.Out, "kept   %s (exists, use --force)\n", target)
			skipped++
			continue
		}
		if err := os.WriteFile(target, data, 0o644); err != nil {
			return err
		}
		fmt.Fprintf(c.Out, "wrote  %s\n", target)
	}
	if skipped > 0 {
		return &exitError{code: 1}
	}
	return nil
}
