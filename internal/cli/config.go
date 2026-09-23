package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"text/tabwriter"

	"dboss/internal/apps"
	"dboss/internal/config"

	"gopkg.in/yaml.v3"
)

// localFlags is the flag set every local command shares: -c/--config and --json.
func (c CLI) localFlags(name string) (*flag.FlagSet, *string, *bool) {
	set := flag.NewFlagSet(name, flag.ContinueOnError)
	set.SetOutput(c.Err)
	return set, configFlag(set), set.Bool("json", false, "JSON output")
}

// config prints a config file as written, or resolved with -d, and reads its saved revisions.
func (c CLI) config(args []string) error {
	set, pathFlag, jsonOutput := c.localFlags("config")
	reference := set.Bool("reference", false, "print the annotated configuration reference")
	keys := set.Bool("keys", false, "list every configuration key with its description and default")
	var withDefaults bool
	set.BoolVar(&withDefaults, "d", false, "include defaults: print the resolved config")
	set.BoolVar(&withDefaults, "defaults", false, "include defaults: print the resolved config")
	operands, err := parseSubcommandFlags(set, args)
	if err != nil {
		return err
	}
	if *reference {
		_, err := io.WriteString(c.Out, config.Reference)
		return err
	}
	if *keys {
		if len(operands) > 1 {
			return errors.New("usage: dboss config --keys [filter]")
		}
		filter := ""
		if len(operands) == 1 {
			filter = operands[0]
		}
		return c.printKeys(filter, *jsonOutput)
	}
	path, err := findConfig(*pathFlag)
	if err != nil {
		return err
	}
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	if len(operands) > 0 && operands[0] == "history" {
		return c.configHistory(cfg, operands[1:], *jsonOutput)
	}
	if len(operands) > 0 && operands[0] == "restore" {
		return c.configRestore(cfg, operands[1:], *jsonOutput)
	}
	if len(operands) > 1 {
		return errors.New("usage: dboss config [app] [-d|--defaults]")
	}
	if len(operands) == 0 {
		if withDefaults {
			return c.dump(cfg, *jsonOutput)
		}
		return c.dumpFile(cfg.SourcePath, *jsonOutput)
	}
	app, err := apps.Lookup(cfg, operands[0])
	if err != nil {
		return err
	}
	if withDefaults {
		return c.dump(app.Config, *jsonOutput)
	}
	appPath, err := config.FindInDir(app.Dir)
	if err != nil {
		return err
	}
	return c.dumpFile(appPath, *jsonOutput)
}

// configHistory lists the saved revisions of a config file: the host file, or one app's file.
func (c CLI) configHistory(cfg config.Config, args []string, jsonOutput bool) error {
	id, err := configID(cfg, args)
	if err != nil {
		return err
	}
	revisions, err := apps.NewStore(cfg).History(id)
	if err != nil {
		return err
	}
	if jsonOutput {
		encoded, _ := json.MarshalIndent(revisions, "", "  ")
		fmt.Fprintln(c.Out, string(encoded))
		return nil
	}
	if len(revisions) == 0 {
		fmt.Fprintln(c.Out, "no saved revisions")
		return nil
	}
	writer := tabwriter.NewWriter(c.Out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(writer, "REVISION\tSAVED\tSOURCE")
	for _, revision := range revisions {
		fmt.Fprintf(writer, "%s\t%s\t%s\n", revision.Revision, revision.Time.Format("2006-01-02 15:04:05"), revision.Source)
	}
	return writer.Flush()
}

// configRestore writes a saved revision back as the current file. The running host picks it up on
// the next rescan.
func (c CLI) configRestore(cfg config.Config, args []string, jsonOutput bool) error {
	if len(args) == 0 {
		return errors.New("usage: dboss config restore [app] <revision>")
	}
	revision := args[len(args)-1]
	id, err := configID(cfg, args[:len(args)-1])
	if err != nil {
		return err
	}
	file, err := apps.NewStore(cfg).Restore(id, revision)
	if err != nil {
		return err
	}
	if jsonOutput {
		encoded, _ := json.MarshalIndent(file, "", "  ")
		fmt.Fprintln(c.Out, string(encoded))
		return nil
	}
	fmt.Fprintf(c.Out, "restored %s to revision %s; run `dboss rescan` to apply it\n", file.ID, revision)
	return nil
}

// configID resolves the history/cache id for the optional app argument: the app's file, or the
// host file inside a host folder.
func configID(cfg config.Config, args []string) (string, error) {
	if len(args) == 0 {
		if cfg.App != nil {
			return "app:" + filepath.Base(cfg.Dir), nil
		}
		return "host", nil
	}
	if len(args) != 1 {
		return "", errors.New("usage: dboss config history|restore [app]")
	}
	app, err := apps.Lookup(cfg, args[0])
	if err != nil {
		return "", err
	}
	return "app:" + app.Name, nil
}

// dumpFile prints a config file as written, comments included; it has already been validated.
func (c CLI) dumpFile(path string, jsonOutput bool) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if !jsonOutput {
		_, err = c.Out.Write(data)
		return err
	}
	var given map[string]any
	if err := yaml.Unmarshal(data, &given); err != nil {
		return err
	}
	return c.dump(given, true)
}

// dump prints a resolved value as 2-space YAML or indented JSON.
func (c CLI) dump(value any, jsonOutput bool) error {
	if jsonOutput {
		data, err := json.MarshalIndent(value, "", "  ")
		if err != nil {
			return err
		}
		_, err = c.Out.Write(append(data, '\n'))
		return err
	}
	encoder := yaml.NewEncoder(c.Out)
	encoder.SetIndent(2)
	if err := encoder.Encode(value); err != nil {
		return err
	}
	return encoder.Close()
}
