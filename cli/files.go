package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/intentd/kome"
	"github.com/intentd/kome/declaration"
)

// loaded is the files given with -f, each validated on its own so messages
// keep their paths, and the migrations they declare.
type loaded struct {
	files []*declaration.File
	// owner names the file each migration comes from.
	owner map[string]*declaration.File
}

// load reads the files, applies --set, validates them, and prints the
// findings to w. It fails on any error finding, or a migration name in two
// files.
func load(paths []string, kindFilter string, sets []string, all bool, w io.Writer) (*loaded, error) {
	if len(paths) == 0 {
		paths = []string{"kome.yaml"}
	}
	l := &loaded{owner: map[string]*declaration.File{}}
	var failed bool
	for _, p := range paths {
		f, err := declaration.Load(p)
		if err != nil {
			return nil, err
		}
		if err := applySets(f, kindFilter, sets, all); err != nil {
			return nil, err
		}
		rep := f.Validate()
		for _, finding := range rep.Findings {
			mark := "error"
			if finding.Warning {
				mark = "warning"
			}
			fmt.Fprintf(w, "%s: %s: %s\n", p, mark, finding)
		}
		if len(rep.Errors()) > 0 {
			failed = true
		}
		for _, name := range f.Names() {
			if prev, dup := l.owner[name]; dup {
				return nil, fmt.Errorf("migration %q is in %s and in %s", name, prev.Dir, f.Dir)
			}
			l.owner[name] = f
		}
		l.files = append(l.files, f)
	}
	if failed {
		return nil, fmt.Errorf("the file has errors")
	}
	return l, nil
}

// applySets applies --set to the migrations --kind selects, or to all with
// --all.
func applySets(f *declaration.File, kindFilter string, sets []string, all bool) error {
	if len(sets) == 0 {
		return nil
	}
	if kindFilter == "" && !all {
		return fmt.Errorf("--set needs --kind or --all")
	}
	for _, name := range f.Names() {
		if !all && !sourceIs(f, name, kindFilter) {
			continue
		}
		for _, s := range sets {
			if err := f.Set(name, s); err != nil {
				return err
			}
		}
	}
	return nil
}

func sourceIs(f *declaration.File, name, kind string) bool {
	id, err := f.Identify(f.Migrations[name].From)
	return err == nil && strings.EqualFold(id.GVK.Kind, kind)
}

// migrations returns the runs of every file, filtered by --kind.
func (l *loaded) migrations(kindFilter string) (kome.Migrations, error) {
	var out kome.Migrations
	for _, f := range l.files {
		ms, err := kome.FromFile(f)
		if err != nil {
			return nil, err
		}
		for _, m := range ms {
			if kindFilter == "" || sourceIs(f, m.Name, kindFilter) {
				out = append(out, m)
			}
		}
	}
	if kindFilter != "" && len(out) == 0 {
		return nil, fmt.Errorf("no migration has source kind %q", kindFilter)
	}
	return out, nil
}

func loadFile(p string) (*declaration.File, error) { return declaration.Load(p) }
