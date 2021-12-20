package view

import (
	_ "embed"

	"gitlab.com/mnm/bud/gen"
	"gitlab.com/mnm/bud/go/mod"
	"gitlab.com/mnm/bud/internal/gotemplate"
	"gitlab.com/mnm/bud/internal/imports"
)

//go:embed view.gotext
var template string

var generator = gotemplate.MustParse("view.gotext", template)

type Generator struct {
	Module *mod.Module
}

type State struct {
	Imports []*imports.Import
}

func (g *Generator) GenerateFile(f gen.F, file *gen.File) error {
	imports := imports.New()
	imports.AddNamed("transform", g.Module.Import("bud/transform"))
	imports.AddNamed("gen", "gitlab.com/mnm/bud/gen")
	imports.AddNamed("mod", "gitlab.com/mnm/bud/go/mod")
	imports.AddNamed("js", "gitlab.com/mnm/bud/js")
	imports.AddNamed("view", "gitlab.com/mnm/bud/view")
	code, err := generator.Generate(State{
		Imports: imports.List(),
	})
	if err != nil {
		return err
	}
	file.Write(code)
	return nil
}