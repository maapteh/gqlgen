package main

import (
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
)

type writer struct {
	extractor
	out    io.Writer
	indent int
}

func write(extractor extractor, out io.Writer) {
	wr := writer{extractor, out, 0}

	wr.writePackage()
	wr.writeImports()
	wr.writeInterface()
	wr.writeVars()
	for _, object := range wr.Objects {
		wr.writeObjectResolver(object)
	}
	wr.writeSchema()
	wr.importHack()
	wr.writeExec()
}

func (w *writer) emit(format string, args ...interface{}) {
	io.WriteString(w.out, fmt.Sprintf(format, args...))
}

func (w *writer) emitIndent() {
	io.WriteString(w.out, strings.Repeat("	", w.indent))
}

func (w *writer) begin(format string, args ...interface{}) {
	w.emitIndent()
	w.emit(format, args...)
	w.lf()
	w.indent++
}

func (w *writer) end(format string, args ...interface{}) {
	w.indent--
	w.emitIndent()
	w.emit(format, args...)
	w.lf()
}

func (w *writer) line(format string, args ...interface{}) {
	w.emitIndent()
	w.emit(format, args...)
	w.lf()
}

func (w *writer) lf() {
	w.out.Write([]byte("\n"))
}

func (w *writer) writePackage() {
	w.line("package %s", w.PackageName)
	w.lf()
}

func (w *writer) writeImports() {
	w.begin("import (")
	for local, pkg := range w.Imports {
		if local == filepath.Base(pkg) {
			w.line(strconv.Quote(pkg))
		} else {
			w.line("%s %s", local, strconv.Quote(pkg))
		}

	}
	w.end(")")
	w.lf()
}

func (w *writer) writeInterface() {
	w.begin("type Resolvers interface {")
	for _, o := range w.Objects {
		for _, f := range o.Fields {
			if f.VarName != "" || f.MethodName != "" {
				continue
			}

			w.emitIndent()
			w.emit("%s_%s(", o.Name, f.GraphQLName)

			w.emit("ctx context.Context")
			if !o.Root {
				w.emit(", it *%s", o.Type.Local())
			}
			for _, arg := range f.Args {
				w.emit(", %s %s", arg.Name, arg.Type.Local())
			}
			w.emit(") (%s, error)", f.Type.Local())
			w.lf()
		}
	}
	w.end("}")
	w.lf()
}

func (w *writer) writeVars() {
	w.begin("var (")
	for _, o := range w.Objects {
		satisfies := strconv.Quote(o.Type.GraphQLName)
		for _, s := range o.satisfies {
			satisfies += ", " + strconv.Quote(s)
		}
		w.line("%sSatisfies = []string{%s}", lcFirst(o.Type.GraphQLName), satisfies)
	}
	w.end(")")
	w.lf()
}

func (w *writer) writeObjectResolver(object object) {
	w.line("// nolint: gocyclo, errcheck, gas, goconst")
	w.begin("func _%s(ec *executionContext, sel []query.Selection, it *%s) {", lcFirst(object.Type.GraphQLName), object.Type.Local())

	w.line("groupedFieldSet := ec.collectFields(sel, %sSatisfies, map[string]bool{})", lcFirst(object.Type.GraphQLName))
	w.line("ec.json.BeginObject()")
	w.begin("for _, field := range groupedFieldSet {")
	w.line("switch field.Name {")

	for _, field := range object.Fields {
		w.begin("case %s:", strconv.Quote(field.GraphQLName))
		w.line("ec.json.ObjectKey(field.Alias)")
		if field.VarName != "" {
			w.writeEvaluateVar(field)
		} else {
			w.writeEvaluateMethod(object, field)
		}

		w.writeJsonType(field.Type, "res")

		w.line("continue")
		w.end("")
	}
	w.line("}")
	w.line(`panic("unknown field " + strconv.Quote(field.Name))`)
	w.end("}")
	w.line("ec.json.EndObject()")

	w.end("}")
	w.lf()
}

func (w *writer) writeEvaluateVar(field Field) {
	w.line("res := %s", field.VarName)
}

func (w *writer) writeEvaluateMethod(object object, field Field) {
	var methodName string
	if field.MethodName != "" {
		methodName = field.MethodName
	} else {
		methodName = fmt.Sprintf("ec.resolvers.%s_%s", object.Name, field.GraphQLName)
	}

	w.writeArgs(field)
	if field.NoErr {
		w.line("res := %s(%s)", methodName, getFuncArgs(object, field))
	} else {
		w.line("res, err := %s(%s)", methodName, getFuncArgs(object, field))
		w.line("if err != nil {")
		w.line("	ec.Error(err)")
		w.line("	continue")
		w.line("}")
	}
}

func (w *writer) writeArgs(field Field) {
	for i, arg := range field.Args {
		w.line("var arg%d %s", i, arg.Type.Local())
		if arg.Type.Basic {
			w.line("if tmp, ok := field.Args[%s]; ok {", strconv.Quote(arg.Name))
			if arg.Type.IsPtr() {
				w.line("	tmp2 := tmp.(%s)", arg.Type.Name)
				w.line("	arg%d = &tmp2", i)
			} else {
				w.line("	arg%d = tmp.(%s)", i, arg.Type.Name)
			}
			w.line("}")
		} else {
			w.line("err := unpackComplexArg(&arg1, field.Args[%s])", strconv.Quote(arg.Name))
			w.line("if err != nil {")
			w.line("	ec.Error(err)")
			w.line("}")
		}
	}

}

func getFuncArgs(object object, field Field) string {
	var args []string

	if field.MethodName == "" {
		args = append(args, "ec.ctx")

		if !object.Root {
			args = append(args, "it")
		}
	}

	for i := range field.Args {
		args = append(args, "arg"+strconv.Itoa(i))
	}

	return strings.Join(args, ", ")
}

func (w *writer) writeJsonType(t kind, val string) {
	w.doWriteJsonType(t, val, t.Modifiers, false)
}

func (w *writer) doWriteJsonType(t kind, val string, remainingMods []string, isPtr bool) {
	for i := 0; i < len(remainingMods); i++ {
		switch remainingMods[i] {
		case modPtr:
			w.begin("if %s == nil {", val)
			w.line("ec.json.Null()")
			w.indent--
			w.begin("} else {")
			w.doWriteJsonType(t, val, remainingMods[i+1:], true)
			w.end("}")
			return
		case modList:
			if isPtr {
				val = "*" + val
			}
			w.line("ec.json.BeginArray()")
			w.begin("for _, val := range %s {", val)
			w.doWriteJsonType(t, "val", remainingMods[i+1:], false)
			w.end("}")
			w.line("ec.json.EndArray()")
			return
		}
	}

	if t.Basic {
		if isPtr {
			val = "*" + val
		}
		w.line("ec.json.%s(%s)", ucFirst(t.Name), val)
	} else if len(t.Implementors) > 0 {
		w.line("switch it := %s.(type) {", val)
		w.line("case nil:")
		w.line("	ec.json.Null()")
		for _, implementor := range t.Implementors {
			w.line("case %s:", implementor.Local())
			w.line("	_%s(ec, field.Selections, &it)", lcFirst(implementor.GraphQLName))
			w.line("case *%s:", implementor.Local())
			w.line("	_%s(ec, field.Selections, it)", lcFirst(implementor.GraphQLName))
		}

		w.line("default:")
		w.line(`	panic(fmt.Errorf("unexpected type %%T", it))`)
		w.line("}")
	} else {
		if !isPtr {
			val = "&" + val
		}
		w.line("_%s(ec, field.Selections, %s)", lcFirst(t.GraphQLName), val)
	}
}

func ucFirst(s string) string {
	r := []rune(s)
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}

func lcFirst(s string) string {
	r := []rune(s)
	r[0] = unicode.ToLower(r[0])
	return string(r)
}

func (w *writer) writeSchema() {
	w.line("var parsedSchema = schema.MustParse(%s)", strconv.Quote(w.schemaRaw))
}

func (w *writer) importHack() {
	w.line("var _ = fmt.Print")
}