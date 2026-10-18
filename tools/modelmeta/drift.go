package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strconv"
	"strings"
)

// genMeta mirrors plugin.modelMeta as read back from a generated file.
type genMeta struct {
	Tier             string
	DisplayName      string
	Description      string
	Underlying       string
	ContextLength    int64
	MaxOutputTokens  int64
	InputModalities  []string
	OutputModalities []string
	Parameters       []string
}

type genCatalog struct {
	meta  map[string]genMeta
	order []string
}

// drift is the graded result of comparing the committed file to a fresh fetch.
//
// Hard drift changes what the plugin exposes or how a request is routed /
// advertised to the host, so --check fails:
//   - an id added or removed (ids carry the namespace, so a namespace move is
//     an add+remove)
//   - Tier changed (isServedModelWithCatalog gates exposure on Tier)
//   - Underlying changed (register.go publishes it as the model Name)
//   - Input/OutputModalities or Parameters changed as a set (published to the
//     host as SupportedModalities / SupportedParameters)
//
// Soft drift only warns: pure order changes (feed order, or order inside a
// slice), ContextLength / MaxOutputTokens changes, and DisplayName /
// Description text. The plugin never clamps or rewrites max_tokens from these
// numbers; they are only advertised metadata, and the plugin's runtime updater
// refreshes them from the live catalog anyway.
type drift struct {
	hard []string
	warn []string
}

func (d drift) clean() bool { return len(d.hard) == 0 && len(d.warn) == 0 }

func rowsToCatalog(rows []row) genCatalog {
	c := genCatalog{meta: map[string]genMeta{}}
	for _, r := range rows {
		if _, dup := c.meta[r.clientID]; !dup {
			c.order = append(c.order, r.clientID)
		}
		c.meta[r.clientID] = genMeta{
			Tier: r.tier, DisplayName: r.displayName, Description: oneLine(r.description),
			Underlying: r.underlying, ContextLength: r.contextLen, MaxOutputTokens: r.maxOutput,
			InputModalities: r.inputMods, OutputModalities: r.outputMods, Parameters: r.params,
		}
	}
	return c
}

// parseGenerated reads generatedCatalog / generatedOrder back out of a file
// produced by render.
func parseGenerated(src string) (genCatalog, error) {
	c := genCatalog{meta: map[string]genMeta{}}
	f, err := parser.ParseFile(token.NewFileSet(), "gen.go", src, 0)
	if err != nil {
		return c, err
	}
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.VAR {
			continue
		}
		for _, spec := range gd.Specs {
			vs := spec.(*ast.ValueSpec)
			if len(vs.Names) != 1 || len(vs.Values) != 1 {
				continue
			}
			lit, ok := vs.Values[0].(*ast.CompositeLit)
			if !ok {
				continue
			}
			switch vs.Names[0].Name {
			case "generatedOrder":
				for _, e := range lit.Elts {
					s, err := strLit(e)
					if err != nil {
						return c, err
					}
					c.order = append(c.order, s)
				}
			case "generatedCatalog":
				for _, e := range lit.Elts {
					kv := e.(*ast.KeyValueExpr)
					id, err := strLit(kv.Key)
					if err != nil {
						return c, err
					}
					m, err := parseMeta(kv.Value.(*ast.CompositeLit))
					if err != nil {
						return c, fmt.Errorf("%s: %w", id, err)
					}
					c.meta[id] = m
				}
			}
		}
	}
	if len(c.meta) == 0 {
		return c, fmt.Errorf("no generatedCatalog found")
	}
	return c, nil
}

func parseMeta(lit *ast.CompositeLit) (genMeta, error) {
	var m genMeta
	for _, e := range lit.Elts {
		kv := e.(*ast.KeyValueExpr)
		name := kv.Key.(*ast.Ident).Name
		var err error
		switch name {
		case "Tier":
			m.Tier, err = strLit(kv.Value)
		case "DisplayName":
			m.DisplayName, err = strLit(kv.Value)
		case "Description":
			m.Description, err = strLit(kv.Value)
		case "Underlying":
			m.Underlying, err = strLit(kv.Value)
		case "ContextLength":
			m.ContextLength, err = intLit(kv.Value)
		case "MaxOutputTokens":
			m.MaxOutputTokens, err = intLit(kv.Value)
		case "InputModalities":
			m.InputModalities, err = sliceLit(kv.Value)
		case "OutputModalities":
			m.OutputModalities, err = sliceLit(kv.Value)
		case "Parameters":
			m.Parameters, err = sliceLit(kv.Value)
		default:
			err = fmt.Errorf("unknown field %s", name)
		}
		if err != nil {
			return m, err
		}
	}
	return m, nil
}

func strLit(e ast.Expr) (string, error) {
	b, ok := e.(*ast.BasicLit)
	if !ok || b.Kind != token.STRING {
		return "", fmt.Errorf("expected string literal")
	}
	return strconv.Unquote(b.Value)
}

func intLit(e ast.Expr) (int64, error) {
	b, ok := e.(*ast.BasicLit)
	if !ok || b.Kind != token.INT {
		return 0, fmt.Errorf("expected int literal")
	}
	return strconv.ParseInt(b.Value, 10, 64)
}

func sliceLit(e ast.Expr) ([]string, error) {
	lit, ok := e.(*ast.CompositeLit)
	if !ok {
		return nil, fmt.Errorf("expected slice literal")
	}
	out := make([]string, 0, len(lit.Elts))
	for _, el := range lit.Elts {
		s, err := strLit(el)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

func compareCatalogs(old, cur genCatalog) drift {
	var d drift
	var added, removed []string
	for id := range cur.meta {
		if _, ok := old.meta[id]; !ok {
			added = append(added, id)
		}
	}
	for id := range old.meta {
		if _, ok := cur.meta[id]; !ok {
			removed = append(removed, id)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	for _, id := range added {
		d.hard = append(d.hard, fmt.Sprintf("model added: %s (tier %s)", id, cur.meta[id].Tier))
	}
	for _, id := range removed {
		d.hard = append(d.hard, fmt.Sprintf("model removed: %s (tier %s)", id, old.meta[id].Tier))
	}

	ids := make([]string, 0, len(cur.meta))
	for id := range cur.meta {
		if _, ok := old.meta[id]; ok {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		o, n := old.meta[id], cur.meta[id]
		if o.Tier != n.Tier {
			d.hard = append(d.hard, fmt.Sprintf("%s: Tier %q -> %q", id, o.Tier, n.Tier))
		}
		if o.Underlying != n.Underlying {
			d.hard = append(d.hard, fmt.Sprintf("%s: Underlying %q -> %q", id, o.Underlying, n.Underlying))
		}
		for _, f := range []struct {
			name string
			a, b []string
		}{
			{"InputModalities", o.InputModalities, n.InputModalities},
			{"OutputModalities", o.OutputModalities, n.OutputModalities},
			{"Parameters", o.Parameters, n.Parameters},
		} {
			switch {
			case !sameSet(f.a, f.b):
				d.hard = append(d.hard, fmt.Sprintf("%s: %s %v -> %v", id, f.name, f.a, f.b))
			case !sameSeq(f.a, f.b):
				d.warn = append(d.warn, fmt.Sprintf("%s: %s order changed", id, f.name))
			}
		}
		if o.ContextLength != n.ContextLength {
			d.warn = append(d.warn, fmt.Sprintf("%s: ContextLength %d -> %d", id, o.ContextLength, n.ContextLength))
		}
		if o.MaxOutputTokens != n.MaxOutputTokens {
			d.warn = append(d.warn, fmt.Sprintf("%s: MaxOutputTokens %d -> %d", id, o.MaxOutputTokens, n.MaxOutputTokens))
		}
		if o.DisplayName != n.DisplayName {
			d.warn = append(d.warn, fmt.Sprintf("%s: DisplayName %q -> %q", id, o.DisplayName, n.DisplayName))
		}
		if o.Description != n.Description {
			d.warn = append(d.warn, fmt.Sprintf("%s: Description changed", id))
		}
	}

	if len(added) == 0 && len(removed) == 0 && !sameSeq(old.order, cur.order) {
		d.warn = append(d.warn, "model order changed (same id set)")
	}
	return d
}

func sameSeq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func sameSet(a, b []string) bool {
	x := append([]string(nil), a...)
	y := append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	return sameSeq(x, y)
}

func formatDrift(d drift) string {
	var b strings.Builder
	for _, h := range d.hard {
		b.WriteString("  HARD " + h + "\n")
	}
	for _, w := range d.warn {
		b.WriteString("  WARN " + w + "\n")
	}
	return b.String()
}
