// Copyright 2019 The go-python Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package bind

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// this version uses pybindgen and a generated .go file to do the binding

// for all preambles: 1 = name of package (outname), 2 = cmdstr

// GoHandle is the type to use for the Handle map key, go-side
// could be a string for more informative but slower handles
var GoHandle = "int64"
var CGoHandle = "C.longlong"
var PyHandle = "int64_t"

// var GoHandle = "string"
// var CGoHandle = "*C.char"
// var PyHandle = "char*"

// 3 = libcfg, 4 = GoHandle, 5 = CGoHandle, 6 = all imports
const (
	goPreamble = `/*
cgo stubs for package %[1]s.
File is generated by gopy. Do not edit.
%[2]s
*/

package main

/*
#cgo pkg-config: %[3]s
#define Py_LIMITED_API
#include <Python.h>
*/
import "C"
import (
	"github.com/goki/gopy/gopyh" // handler
	%[6]s
)

func main() {}

// type for the handle -- int64 for speed (can switch to string)
type GoHandle %[4]s
type CGoHandle %[5]s

// boolGoToPy converts a Go bool to python-compatible C.char
func boolGoToPy(b bool) C.char {
	if b {
		return 1
	}
	return 0
}

// boolPyToGo converts a python-compatible C.Char to Go bool
func boolPyToGo(b C.char) bool {
	if b != 0 {
		return true
	}
	return false
}

`

	PyBuildPreamble = `# python build stubs for package %[1]s
# File is generated by gopy. Do not edit.
# %[2]s

from pybindgen import retval, param, Module
import sys

mod = Module('_%[1]s')
mod.add_include('"%[1]s_go.h"')
`

	// 3 = specific package name, 4 = spec pkg path, 5 = doc, 6 = imports
	PyWrapPreamble = `%[5]s
# python wrapper for package %[4]s within overall package %[1]s
# This is what you import to use the package.
# File is generated by gopy. Do not edit.
# %[2]s

# the following is required to enable dlopen to open the _go.so file
import os,sys,inspect,collections
cwd = os.getcwd()
currentdir = os.path.dirname(os.path.abspath(inspect.getfile(inspect.currentframe())))
os.chdir(currentdir)
import _%[1]s
os.chdir(cwd)

# to use this code in your end-user python file, import it as follows:
# from %[1]s import %[3]s
# and then refer to everything using %[3]s. prefix
# packages imported by this package listed below:

%[6]s

class GoClass(object):
	"""GoClass is the base class for all GoPy wrapper classes"""
	pass
	
`

	// 3 = gencmd, 4 = vm, 5 = libext
	MakefileTemplate = `# Makefile for python interface for package %[1]s.
# File is generated by gopy. Do not edit.
# %[2]s

GOCMD=go
GOBUILD=$(GOCMD) build
PYTHON=%[4]s
PYTHON_CFG=$(PYTHON)-config
GCC=gcc
LIBEXT=%[5]s

# get the flags used to build python:
CFLAGS = $(shell $(PYTHON_CFG) --cflags)
LDFLAGS = $(shell $(PYTHON_CFG) --ldflags)

all: gen build

gen:
	%[3]s

build:
	# build target builds the generated files -- this is what gopy build does..
	# this will otherwise be built during go build and may be out of date
	- rm %[1]s.c
	# generate %[1]s_go$(LIBEXT) from %[1]s.go -- the cgo wrappers to go functions
	$(GOBUILD) -buildmode=c-shared -ldflags="-s -w" -o %[1]s_go$(LIBEXT) %[1]s.go
	# use pybindgen to build the %[1]s.c file which are the CPython wrappers to cgo wrappers..
	# note: pip install pybindgen to get pybindgen if this fails
	$(PYTHON) build.py
	# build the _%[1]s$(LIBEXT) library that contains the cgo and CPython wrappers
	# generated %[1]s.py python wrapper imports this c-code package
	$(GCC) %[1]s.c -dynamiclib %[1]s_go$(LIBEXT) -o _%[1]s$(LIBEXT) $(CFLAGS) $(LDFLAGS)
	
`
)

// actually, the weird thing comes from adding symbols to the output, even with dylib!
// ifeq ($(LIBEXT), ".dylib")
// 	# python apparently doesn't recognize .dylib on mac, but .so causes extra weird dylib dir..
// 	- ln -s _%[1]s$(LIBEXT) _%[1]s.so
// endif
//

// GenPyBind generates a .go file, build.py file to enable pybindgen to create python bindings,
// and wrapper .py file(s) that are loaded as the interface to the package with shadow
// python-side classes
func GenPyBind(odir, outname, cmdstr, vm, libext string, lang int) error {
	gen := &pyGen{
		odir:    odir,
		outname: outname,
		cmdstr:  cmdstr,
		vm:      vm,
		libext:  libext,
		lang:    lang,
	}
	err := gen.gen()
	if err != nil {
		return err
	}
	return err
}

type pyGen struct {
	gofile   *printer
	pybuild  *printer
	pywrap   *printer
	makefile *printer

	pkg    *Package // current package (only set when doing package-specific processing)
	err    ErrorList
	pkgmap map[string]struct{} // map of package paths

	odir    string // output directory
	outname string // overall output (package) name
	cmdstr  string // overall command (embedded in generated files)
	vm      string // python interpreter
	libext  string
	lang    int // c-python api version (2,3)
}

func (g *pyGen) gen() error {
	g.pkg = nil
	err := os.MkdirAll(g.odir, 0755)
	if err != nil {
		return fmt.Errorf("gopy: could not create output directory: %v", err)
	}

	g.genPackageMap()
	g.genPre()
	g.genExtTypesGo()
	for _, p := range Packages {
		g.genPkg(p)
	}
	g.genOut()
	if len(g.err) == 0 {
		return nil
	}
	return g.err.Error()
}

func (g *pyGen) genPackageMap() {
	g.pkgmap = make(map[string]struct{})
	for _, p := range Packages {
		g.pkgmap[p.pkg.Path()] = struct{}{}
	}
}

func (g *pyGen) genPre() {
	g.gofile = &printer{buf: new(bytes.Buffer), indentEach: []byte("\t")}
	g.pybuild = &printer{buf: new(bytes.Buffer), indentEach: []byte("\t")}
	g.makefile = &printer{buf: new(bytes.Buffer), indentEach: []byte("\t")}
	g.genGoPreamble()
	g.genPyBuildPreamble()
	g.genMakefile()
	oinit, err := os.Create(filepath.Join(g.odir, "__init__.py"))
	g.err.Add(err)
	oinit.Close()
}

func (g *pyGen) genPrintOut(outfn string, pr *printer) {
	of, err := os.Create(filepath.Join(g.odir, outfn))
	g.err.Add(err)
	_, err = io.Copy(of, pr)
	g.err.Add(err)
	of.Close()
}

func (g *pyGen) genOut() {
	g.pybuild.Printf("\nmod.generate(open('%v.c', 'w'))\n\n", g.outname)
	g.gofile.Printf("\n\n")
	g.makefile.Printf("\n\n")
	g.genPrintOut(g.outname+".go", g.gofile)
	g.genPrintOut("build.py", g.pybuild)
	g.genPrintOut("Makefile", g.makefile)
}

func (g *pyGen) genPkgWrapOut() {
	g.pywrap.Printf("\n\n")
	g.genPrintOut(g.pkg.pkg.Name()+".py", g.pywrap)
}

func (g *pyGen) genPkg(p *Package) {
	g.pkg = p
	g.pywrap = &printer{buf: new(bytes.Buffer), indentEach: []byte("\t")}
	g.genPyWrapPreamble()
	g.genExtTypesPyWrap()
	g.genAll()
	g.genPkgWrapOut()
	g.pkg = nil
}

func (g *pyGen) genGoPreamble() {
	pkgimport := ""
	for pi, _ := range current.imports {
		pkgimport += fmt.Sprintf("\n\t%q", pi)
	}
	pypath, pyonly := filepath.Split(g.vm)
	pyroot, _ := filepath.Split(filepath.Clean(pypath))
	libcfg := filepath.Join(filepath.Join(filepath.Join(pyroot, "lib"), "pkgconfig"), pyonly+".pc")
	g.gofile.Printf(goPreamble, g.outname, g.cmdstr, libcfg, GoHandle, CGoHandle, pkgimport)
	g.gofile.Printf("\n// --- generated code for package: %[1]s below: ---\n\n", g.outname)
}

func (g *pyGen) genPyBuildPreamble() {
	g.pybuild.Printf(PyBuildPreamble, g.outname, g.cmdstr)
}

func (g *pyGen) genPyWrapPreamble() {
	n := g.pkg.pkg.Name()
	pkgimport := g.pkg.pkg.Path()
	pkgDoc := g.pkg.doc.Doc
	if pkgDoc != "" {
		pkgDoc = `"""` + "\n" + pkgDoc + "\n" + `"""`
	}

	// import other packages for other types that we might use
	impstr := ""
	imps := g.pkg.pkg.Imports()
	for _, im := range imps {
		ipath := im.Path()
		if _, has := g.pkgmap[ipath]; has {
			impstr += fmt.Sprintf("from %s import %s\n", g.outname, im.Name())
		}
	}

	g.pywrap.Printf(PyWrapPreamble, g.outname, g.cmdstr, n, pkgimport, pkgDoc, impstr)
}

// StripOutputFromCmd removes -output from command -- needed for putting in
// Makefiles
func StripOutputFromCmd(cmdstr string) string {
	if oidx := strings.Index(cmdstr, "-output="); oidx > 0 {
		spidx := strings.Index(cmdstr[oidx:], " ")
		cmdstr = cmdstr[:oidx] + cmdstr[oidx+spidx+1:]
	}
	return cmdstr
}

func (g *pyGen) genMakefile() {
	gencmd := strings.Replace(g.cmdstr, "gopy build", "gopy gen", 1)
	gencmd = StripOutputFromCmd(gencmd)
	g.makefile.Printf(MakefileTemplate, g.outname, g.cmdstr, gencmd, g.vm, g.libext)
}

// generate external types, go code
func (g *pyGen) genExtTypesGo() {
	g.gofile.Printf("\n// ---- External Types Outside of Targeted Packages ---\n")

	names := current.names()
	for _, n := range names {
		sym := current.sym(n)
		if !sym.isType() {
			continue
		}
		if _, has := g.pkgmap[sym.gopkg.Path()]; has {
			continue
		}
		g.genType(sym, true, false) // ext types, no python wrapping
	}
}

// generate external types, py wrap
func (g *pyGen) genExtTypesPyWrap() {
	g.pywrap.Printf("\n# ---- External Types Outside of Targeted Packages ---\n")

	names := current.names()
	for _, n := range names {
		sym := current.sym(n)
		if !sym.isType() {
			continue
		}
		if _, has := g.pkgmap[sym.gopkg.Path()]; has {
			continue
		}
		g.genType(sym, true, true) // ext types, only python wrapping
	}
}

func (g *pyGen) genAll() {
	g.gofile.Printf("\n// ---- Package: %s ---\n", g.pkg.Name())

	g.gofile.Printf("\n// ---- Types ---\n")
	g.pywrap.Printf("\n# ---- Types ---\n")
	names := current.names()
	for _, n := range names {
		sym := current.sym(n)
		if !sym.isType() || sym.gopkg != g.pkg.pkg {
			continue
		}
		g.genType(sym, false, false) // not exttypes
	}

	g.pywrap.Printf("\n\n#---- Constants from Go: Python can only ask that you please don't change these! ---\n")
	for _, c := range g.pkg.consts {
		g.genConst(c)
	}

	g.gofile.Printf("\n\n// ---- Global Variables: can only use functions to access ---\n")
	g.pywrap.Printf("\n\n# ---- Global Variables: can only use functions to access ---\n")
	for _, v := range g.pkg.vars {
		g.genVar(v)
	}

	g.gofile.Printf("\n\n// ---- Interfaces ---\n")
	g.pywrap.Printf("\n\n# ---- Interfaces ---\n")
	for _, ifc := range g.pkg.ifaces {
		g.genInterface(ifc)
	}

	g.gofile.Printf("\n\n// ---- Structs ---\n")
	g.pywrap.Printf("\n\n# ---- Structs ---\n")
	for _, s := range g.pkg.structs {
		g.genStruct(s)
	}

	// note: these are extracted from reg functions that return full
	// type (not pointer -- should do pointer but didn't work yet)
	g.gofile.Printf("\n\n// ---- Constructors ---\n")
	g.pywrap.Printf("\n\n# ---- Constructors ---\n")
	for _, s := range g.pkg.structs {
		for _, ctor := range s.ctors {
			g.genFunc(ctor)
		}
	}

	g.gofile.Printf("\n\n// ---- Functions ---\n")
	g.pywrap.Printf("\n\n# ---- Functions ---\n")
	for _, f := range g.pkg.funcs {
		g.genFunc(f)
	}
}

func (g *pyGen) genConst(c Const) {
	g.genConstValue(c)
}

func (g *pyGen) genVar(v Var) {
	g.genVarGetter(v)
	g.genVarSetter(v)
}
