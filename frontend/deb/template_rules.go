package deb

import (
	"bytes"
	_ "embed"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/Azure/dalec"
	"github.com/moby/buildkit/client/llb"
	"golang.org/x/exp/maps"
)

func Rules(spec *dalec.Spec, in llb.State, dir string) (llb.State, error) {
	buf := bytes.NewBuffer(nil)

	if dir == "" {
		dir = "debian"
	}

	if err := WriteRules(spec, buf); err != nil {
		return llb.Scratch(), err
	}

	return in.
			File(llb.Mkdir(dir, 0o755, llb.WithParents(true))).
			File(llb.Mkfile(filepath.Join(dir, "rules"), 0o700, buf.Bytes())),
		nil
}

func WriteRules(spec *dalec.Spec, w io.Writer) error {
	return rulesTmpl.Execute(w, &rulesWrapper{spec})
}

type rulesWrapper struct {
	*dalec.Spec
}

func (w *rulesWrapper) Envs() fmt.Stringer {
	b := &strings.Builder{}

	for k, v := range w.Spec.Build.Env {
		fmt.Fprintf(b, "export %s := %s\n", k, v)
	}

	if w.Spec.HasGomods() {
		fmt.Fprintf(b, "export %s := $(PWD)/%s\n", "GOMODCACHE", gomodsName)
	}

	return b
}

func (w *rulesWrapper) OverridePerms() fmt.Stringer {
	b := &strings.Builder{}

	if w.Artifacts.Directories == nil {
		return b
	}

	var fixPerms bool
	for _, cfg := range w.Artifacts.Directories.Config {
		if cfg.Mode != 0 {
			fixPerms = true
			break
		}
	}

	if !fixPerms {
		for _, cfg := range w.Artifacts.Directories.State {
			if cfg.Mode != 0 {
				fixPerms = true
				break
			}
		}
	}

	if fixPerms {
		b.WriteString("execute_after_dh_fixperms:\n")
		b.WriteString("\tdebian/dalec/fix_perms.sh\n\n")
	}

	return b
}

// groupUnitsByBaseName indexes the provided list by the unit basename.
// A unit basename is the name of the unit without the suffix (e.g. ".service", ".socket", etc).
// The nested map is key'd on the fully resolved unit name.
func groupUnitsByBaseName(ls map[string]dalec.SystemdUnitConfig) map[string]map[string]dalec.SystemdUnitConfig {
	idx := make(map[string]map[string]dalec.SystemdUnitConfig)
	for k, v := range ls {
		base, suffix := v.SplitName(k)
		if idx[base] == nil {
			idx[base] = make(map[string]dalec.SystemdUnitConfig)
		}
		idx[base][base+"."+suffix] = v
	}

	return idx
}

func (w *rulesWrapper) OverrideSystemd() (fmt.Stringer, error) {
	b := &strings.Builder{}

	units := w.Spec.Artifacts.Systemd.GetUnits()

	if len(units) == 0 {
		return b, nil
	}

	b.WriteString("override_dh_installsystemd:\n")

	grouped := groupUnitsByBaseName(units)
	sorted := dalec.SortMapKeys(grouped)

	var includeCustomEnable bool
	for _, basename := range sorted {
		grouping := grouped[basename]

		needsCustomEnable := requiresCustomEnable(grouping)
		if needsCustomEnable {
			includeCustomEnable = true
		}

		// dh_installsystemd does not want the suffix of the file, so trim it off
		// here.
		// Otherwise it will _silently_ fail, *yay*.
		// We also need to check if there are multiple units with the same base name
		// with different `Enable` options set.
		// `dh_installsystemd` cannot deal with this, in those cases we'll write a
		// custom postinst/postrm script.
		//
		// We also only need to do this once per basename, so we don't need to
		// itterate over every unit.

		// Get the first key which we'll use to check if the unit is enabled.
		// Either all units are enabled or not enabled OR we need to do custom enable
		firstKey := maps.Keys(grouping)[0]
		enable := grouping[firstKey].Enable

		b.WriteString("\tdh_installsystemd --name=" + basename)
		if !enable || needsCustomEnable {
			b.WriteString(" --no-enable")
		}
		b.WriteString("\n")
	}

	if includeCustomEnable {
		b.WriteString("\t[ -f debian/postinst ] || (echo '#!/bin/sh' > debian/postinst; echo 'set -e' >> debian/postinst)\n")
		b.WriteString("\t[ -x debian/postinst ] || chmod +x debian/postinst\n")
		b.WriteString("\tcat debian/dalec/" + customSystemdPostinstFile + " >> debian/postinst\n")
	}

	return b, nil
}

var (
	//go:embed templates/debian_rules.tmpl
	rulesTmplContent []byte

	rulesTmpl = template.Must(template.New("rules").Parse(string(rulesTmplContent)))
)
