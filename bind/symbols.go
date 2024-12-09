// Copyright 2015 The go-python Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package bind

import (
	"fmt"
	"go/types"
	"hash/fnv"
	"sort"
	"strings"
	"sync"
)

var (
	universeMutex sync.Mutex
	universe      *symtab // universe contains Go global types that are not generated
	current       *symtab // current contains all symbols from current multi-package gen run
)

func hash(s string) string {
	h := fnv.New32a()
	h.Write([]byte(s))
	return fmt.Sprintf("0x%d", h.Sum32())
}

// symkind describes the kinds of symbol
type symkind int

const (
	skConst symkind = 1 << iota
	skVar
	skFunc
	skType
	skArray
	skBasic
	skInterface
	skMap
	skNamed
	skPointer
	skSignature
	skSlice
	skStruct
	skString
)

var (
	symkinds = map[string]symkind{
		"const":     skConst,
		"var":       skVar,
		"func":      skFunc,
		"type":      skType,
		"array":     skArray,
		"basic":     skBasic,
		"interface": skInterface,
		"map":       skMap,
		"named":     skNamed,
		"pointer":   skPointer,
		"signature": skSignature,
		"slice":     skSlice,
		"struct":    skStruct,
		"string":    skString,
	}
)

func (k symkind) String() string {
	str := []string{}
	for n, v := range symkinds {
		if (k & v) != 0 {
			str = append(str, n)
		}
	}
	sort.Strings(str)
	return strings.Join(str, "|")
}

var pyKeywords = map[string]struct{}{
	"False": struct{}{}, "None": struct{}{}, "True": struct{}{}, "and": struct{}{}, "as": struct{}{}, "assert": struct{}{}, "break": struct{}{}, "class": struct{}{}, "continue": struct{}{}, "def": struct{}{}, "del": struct{}{}, "elif": struct{}{}, "else": struct{}{}, "except": struct{}{}, "finally": struct{}{}, "for": struct{}{}, "from": struct{}{}, "global": struct{}{}, "if": struct{}{}, "import": struct{}{}, "in": struct{}{}, "is": struct{}{}, "lambda": struct{}{}, "nonlocal": struct{}{}, "not": struct{}{}, "or": struct{}{}, "pass": struct{}{}, "raise": struct{}{}, "return": struct{}{}, "try": struct{}{}, "while": struct{}{}, "with": struct{}{}, "yield": struct{}{}, "self": struct{}{},
}

// pySafeName returns a name that python will not barf on
func pySafeName(nm string) string {
	if _, bad := pyKeywords[nm]; bad {
		return "my" + nm
	}
	return nm
}

// pySafeArg returns an arg name that python will not barf on
func pySafeArg(anm string, idx int) string {
	if anm == "" {
		anm = fmt.Sprintf("arg_%d", idx)
	}
	return pySafeName(anm)
}

// isPyCompatVar checks if var is compatible with python
func isPyCompatVar(v *symbol) error {
	if v == nil {
		return fmt.Errorf("gopy: var symbol not found")
	}
	if v.isPointer() && v.isBasic() {
		return fmt.Errorf("gopy: var is pointer to basic type")
	}
	if isErrorType(v.gotyp) {
		return fmt.Errorf("gopy: var is error type")
	}
	if _, isChan := v.gotyp.(*types.Chan); isChan {
		return fmt.Errorf("gopy: var is channel type")
	}
	return nil
}

// isPyCompatType checks if type is compatible with python
func isPyCompatType(typ types.Type) error {
	typ = typ.Underlying()
	if ptyp, isPtr := typ.(*types.Pointer); isPtr {
		if _, isBasic := ptyp.Elem().(*types.Basic); isBasic {
			return fmt.Errorf("gopy: type is pointer to basic type")
		}
	}
	if isErrorType(typ) {
		return fmt.Errorf("gopy: type is error type")
	}
	if _, isChan := typ.(*types.Chan); isChan {
		return fmt.Errorf("gopy: type is channel type")
	}
	return nil
}

// isPyCompatField checks if field is compatible with python
func isPyCompatField(f *types.Var) (*symbol, error) {
	if !f.Exported() || f.Embedded() {
		return nil, fmt.Errorf("gopy: field not exported or is embedded")
	}
	ftyp := current.symtype(f.Type())
	if _, isSig := f.Type().Underlying().(*types.Signature); isSig {
		return nil, fmt.Errorf("gopy: type is function signature")
	}
	if f.Type().Underlying().String() == "interface{}" {
		return nil, fmt.Errorf("gopy: type is interface{}")
	}
	return ftyp, isPyCompatVar(ftyp)
}

// isPyCompatFunc checks if function signature is a python-compatible function.
// Returns nil if function is compatible, err message if not.
// Also returns the return type of the function
// haserr is true if 2nd arg is an error type, which is only
// supported form of multi-return-value functions
// hasfun is true if one of the args is a function signature
func isPyCompatFunc(sig *types.Signature) (ret types.Type, haserr, hasfun bool, err error) {
	res := sig.Results()

	switch res.Len() {
	case 2:
		if !isErrorType(res.At(1).Type()) {
			err = fmt.Errorf("gopy: second result value must be of type error: %s", sig.String())
			return
		}
		haserr = true
		ret = res.At(0).Type()
	case 1:
		if isErrorType(res.At(0).Type()) {
			haserr = true
			ret = nil
		} else {
			ret = res.At(0).Type()
		}
	case 0:
		ret = nil
	default:
		err = fmt.Errorf("gopy: too many results to return: %s", sig.String())
		return
	}

	if ret != nil {
		if err = isPyCompatType(ret); err != nil {
			return
		}
		if _, isSig := ret.Underlying().(*types.Signature); isSig {
			err = fmt.Errorf("gopy: return type is signature")
			return
		}
		if ret.Underlying().String() == "interface{}" {
			err = fmt.Errorf("gopy: return type is interface{}")
			return
		}
	}

	args := sig.Params()
	nargs := args.Len()
	for i := 0; i < nargs; i++ {
		arg := args.At(i)
		argt := arg.Type()
		if err = isPyCompatType(argt); err != nil {
			return
		}
		if _, isSig := argt.Underlying().(*types.Signature); isSig {
			if !hasfun {
				hasfun = true
			} else {
				err = fmt.Errorf("gopy: only one function signature arg allowed: %s", sig.String())
				return
			}
		}
	}
	return
}

// symbol is an exported symbol in a go package
type symbol struct {
	kind         symkind
	gopkg        *types.Package
	goobj        types.Object
	gotyp        types.Type
	doc          string
	id           string // canonical name for referring to the symbol as a valid variable / function name etc
	goname       string // go formatted type name (gofmt)
	cgoname      string // type name of entity for cgo -- handle for objs
	cpyname      string // type name of entity for cgo - python -- handle..
	pysig        string // type string for doc-signatures
	go2py        string // name of go->py converter function
	go2pyParenEx string // extra parentheses needed at end of go2py (beyond 1 default)
	py2go        string // name of py->go converter function
	py2goParenEx string // extra parentheses needed at end of py2go (beyond 1 default)
	zval         string // zero value representation
	pyfmt        string // python Py_BuildValue / Py_ParseTuple formatting string
}

func isPrivate(s string) bool {
	return (strings.ToLower(s[0:1]) == s[0:1])
}

func (s *symbol) isType() bool {
	return (s.kind & skType) != 0
}

func (s *symbol) isNamed() bool {
	return (s.kind & skNamed) != 0
}

func (s *symbol) isBasic() bool {
	return (s.kind & skBasic) != 0
}

func (s *symbol) isNamedBasic() bool {
	if !s.isNamed() {
		return false
	}
	_, ok := s.gotyp.Underlying().(*types.Basic)
	if ok {
		return true
	}
	return false
}

func (s *symbol) isArray() bool {
	return (s.kind & skArray) != 0
}

func (s *symbol) isInterface() bool {
	return (s.kind & skInterface) != 0
}

func (s *symbol) isSignature() bool {
	return (s.kind & skSignature) != 0
}

func (s *symbol) isMap() bool {
	return (s.kind & skMap) != 0
}

func (s *symbol) isPySequence() bool {
	return s.isArray() || s.isSlice() || s.isMap()
}

func (s *symbol) isSlice() bool {
	return (s.kind & skSlice) != 0
}

func (s *symbol) isStruct() bool {
	return (s.kind & skStruct) != 0
}

func (s *symbol) isPointer() bool {
	return (s.kind & skPointer) != 0
}

func (s *symbol) isPtrOrIface() bool {
	return s.isPointer() || s.isInterface()
}

func (s *symbol) hasHandle() bool {
	if s.goname == "interface{}" {
		return false
	}
	return !s.isBasic() && !s.isSignature()
}

func (s *symbol) hasConverter() bool {
	return (s.go2py != "" || s.py2go != "")
}

func (s *symbol) pkgname() string {
	if s.gopkg == nil {
		return ""
	}
	return s.gopkg.Name()
}

func (s *symbol) GoType() types.Type {
	if s.goobj != nil {
		return s.goobj.Type()
	}
	return s.gotyp
}

func (s *symbol) cgotypename() string {
	typ := s.gotyp
	switch typ := typ.(type) {
	case *types.Basic:
		n := typ.Name()
		if strings.HasPrefix(n, "untyped ") {
			n = string(n[len("untyped "):])
		}
		return n
	case *types.Named:
		obj := s.goobj
		switch typ.Underlying().(type) {
		case *types.Struct:
			return s.cgoname
		case *types.Interface:
			if obj.Name() == "error" {
				return "error"
			}
		}
	}
	return s.cgoname
}

// pyPkgId returns the python package-qualified version of Id
func (s *symbol) pyPkgId(curPkg *types.Package) string {
	pnm := s.gopkg.Name()
	ppath := s.gopkg.Path()
	if _, has := thePyGen.pkgmap[ppath]; !has { // external symbols are all in go package
		if pnm == "go" {
			return s.id
		} else {
			return "go." + s.id
		}
	}
	if pnm == "go" {
		return pnm + "." + s.id
	}
	if !s.isNamed() && (s.isMap() || s.isSlice() || s.isArray()) {
		//		idnm := strings.TrimPrefix(s.id[uidx+1:], pnm+"_") // in case it has that redundantly
		if ppath != curPkg.Path() {
			thePyGen.pkg.AddPyImport(ppath, true) // ensure that this is included in current package
			return pnm + "." + s.id
		} else {
			return s.id
		}
	}
	uidx := strings.Index(s.id, "_")
	if uidx < 0 {
		return s.id // shouldn't happen
	}
	idnm := strings.TrimPrefix(s.id[uidx+1:], pnm+"_") // in case it has that redundantly
	if ppath != curPkg.Path() {
		thePyGen.pkg.AddPyImport(ppath, true) // ensure that this is included in current package
		return pnm + "." + idnm
	} else {
		return idnm
	}
}

//////////////////////////////////////////////////////////////
// symtab

// symtab is a table of symbols in a go package
type symtab struct {
	pkg         *types.Package // current package only -- can change depending..
	syms        map[string]*symbol
	imports     map[string]string // key is full path, value is unique name
	importNames map[string]string // package name to path map -- for detecting name conflicts
	uniqName    int               // index for making package name unique
	parent      *symtab
}

func newSymtab(pkg *types.Package, parent *symtab) *symtab {
	if parent == nil {
		parent = universe
	}
	s := &symtab{
		pkg:         pkg,
		syms:        make(map[string]*symbol),
		imports:     make(map[string]string),
		importNames: make(map[string]string),
		parent:      parent,
	}
	return s
}

func (sym *symtab) names() []string {
	names := make([]string, 0, len(sym.syms))
	for n := range sym.syms {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func (sym *symtab) sym(n string) *symbol {
	s, ok := sym.syms[n]
	if ok {
		return s
	}
	if sym.parent != nil {
		return sym.parent.sym(n)
	}
	return nil
}

// adds package to imports if not already on it, and returns
// unique name used to refer to it
func (sym *symtab) addImport(pkg *types.Package) string {
	p := pkg.Path()
	nm := pkg.Name()
	unm := nm
	ep, exists := sym.imports[p]
	if exists {
		return ep
	}
	ep, exists = sym.importNames[nm]
	if exists && ep != p {
		if exists && ep != p {
			sym.uniqName++
			unm = fmt.Sprintf("s%d_%s", sym.uniqName, nm)
			fmt.Printf("import conflict: existing: %s  new: %s  alias: %s\n", ep, p, unm)
		}
	}
	sym.importNames[unm] = p
	sym.imports[p] = unm
	return unm
}

func (sym *symtab) typeof(n string) *symbol {
	s := sym.sym(n)
	switch s.kind {
	case skVar, skConst:
		tname := sym.fullTypeString(s.goobj.Type())
		return sym.sym(tname)
	case skFunc:
		//FIXME(sbinet): really?
		return s
	case skType:
		return s
	default:
		panic(fmt.Errorf("unhandled symbol kind (%v)", s.kind))
	}
}

// fullTypeString returns the fully-qualified type string with entire package import path
func (sym *symtab) fullTypeString(t types.Type) string {
	return types.TypeString(t, nil)
}

func (sym *symtab) symtype(t types.Type) *symbol {
	tname := sym.fullTypeString(t)
	return sym.sym(tname)
}

// typePkg gets the package for a given types.Type
func (sym *symtab) typePkg(t types.Type) *types.Package {
	if tn, ok := t.(*types.Named); ok {
		return tn.Obj().Pkg()
	}
	switch tt := t.(type) {
	case *types.Pointer:
		return sym.typePkg(tt.Elem())
	}
	return nil
}

// typeGoName returns the go type name that is always qualified by an appropriate package name
// this should always be used for "goname" in general.
func (sym *symtab) typeGoName(t types.Type) string {
	return types.TypeString(t, func(pkg *types.Package) string {
		pnm := sym.addImport(pkg) // always make sure
		return pnm
	})
}

// typeIdName returns typeGoName with . -> _ -- this should always be used for id
func (sym *symtab) typeIdName(t types.Type) string {
	idn := strings.Replace(sym.typeGoName(t), ".", "_", -1)
	if _, isary := t.(*types.Array); isary {
		idn = strings.Replace(idn, "[", "Array_", 1)
		idn = strings.Replace(idn, "]", "_", 1)
	}
	idn = strings.Replace(idn, "[]", "Slice_", -1)
	idn = strings.Replace(idn, "map[", "Map_", -1)
	idn = strings.Replace(idn, "[", "_", -1)
	idn = strings.Replace(idn, "]", "_", -1)
	idn = strings.Replace(idn, "{}", "_", -1)
	idn = strings.Replace(idn, "*", "Ptr_", -1)
	return idn
}

func (sym *symtab) addSymbol(obj types.Object) error {
	fn := types.ObjectString(obj, nil)
	n := obj.Name()
	pkg := obj.Pkg()
	id := n
	var pkgnm string
	if pkg != nil {
		pkgnm = sym.importNames[pkg.Path()]
		id = pkgnm + "_" + n
	}
	switch obj.(type) {
	case *types.Const:
		sym.syms[fn] = &symbol{
			gopkg:   pkg,
			goobj:   obj,
			kind:    skConst,
			id:      id,
			goname:  n,
			cgoname: n, // TODO: type names!
			cpyname: n,
		}
		tsym := sym.symtype(obj.Type())
		if tsym == nil {
			return sym.addType(obj, obj.Type())
		}

	case *types.Var:
		sym.syms[fn] = &symbol{
			gopkg:   pkg,
			goobj:   obj,
			kind:    skVar,
			id:      id,
			goname:  n,
			cgoname: n,
			cpyname: n,
		}
		tsym := sym.symtype(obj.Type())
		if tsym == nil {
			return sym.addType(obj, obj.Type())
		}

	case *types.Func:
		sig := obj.Type().Underlying().(*types.Signature)
		_, _, _, err := isPyCompatFunc(sig)
		if err == nil {
			sym.syms[fn] = &symbol{
				gopkg:   pkg,
				goobj:   obj,
				kind:    skFunc,
				id:      id,
				goname:  n,
				cgoname: n,
				cpyname: n,
			}
			err := sym.processTuple(sig.Params())
			if err != nil {
				return err
			}
			return sym.processTuple(sig.Results())
		}
		if !NoWarn {
			fmt.Printf("ignoring python incompatible function: %v.%v: %v: %v\n", pkgnm, obj.String(), sig.String(), err)
		}

	case *types.TypeName:
		// tn := obj.(*types.TypeName)
		// if tn.IsAlias() {
		// 	fmt.Printf("type is an alias: %v\n", tn)
		// }
		return sym.addType(obj, obj.Type())

	default:
		return fmt.Errorf("gopy: handled object [%#v]", obj)
	}
	return nil
}

// processTuple ensures that all types in a tuple are in sym table
func (sym *symtab) processTuple(tuple *types.Tuple) error {
	if tuple == nil {
		return nil
	}
	for i := 0; i < tuple.Len(); i++ {
		ivar := tuple.At(i)
		ityp := ivar.Type()
		isym := sym.symtype(ityp)
		if isym != nil {
			continue
		}
		err := sym.addType(ivar, ityp)
		if err != nil {
			return err
		}
	}
	return nil
}

// buildTuple returns a string of Go code that builds a PyTuple
// for the given tuple (e.g., function args).
// varnm is the name of the local variable for the built tuple
// if methvar is non-empty, check if given PyObject* variable is a method,
// and if so, add the self arg for it in the first position, and set the var
// to be the actual method function.
func (sym *symtab) buildTuple(tuple *types.Tuple, varnm string, methvar string) (string, error) {
	sz := tuple.Len()
	if sz == 0 {
		return "", fmt.Errorf("buildTuple: no elements")
	}
	// TODO: https://www.reddit.com/r/Python/comments/3618cd/calling_back_python_instance_methods_from_c/
	// could not get this to work across threads for methods -- and furthermore the basic version with
	// CallObject works fine within the same thread, so all this extra work seems unnecessary.
	//
	// bstr := fmt.Sprintf("var %s *C.PyObject\n", varnm)
	// bstr += fmt.Sprintf("_pyargstidx := 0\n")
	// bstr += fmt.Sprintf("_pyargidx := C.long(0)\n")
	// if methvar != "" {
	// 	bstr += fmt.Sprintf("if C.gopy_method_check(%s) != 0 {\n", methvar)
	// 	bstr += fmt.Sprintf("\tC.gopy_incref(%s)\n", methvar)
	// 	bstr += fmt.Sprintf("\t%s = C.PyTuple_New(%d)\n", varnm, sz+1)
	// 	bstr += fmt.Sprintf("\tC.PyTuple_SetItem(%s, 0, C.PyMethod_Self(%s))\n", varnm, methvar)
	// 	bstr += fmt.Sprintf("\t_pyargstidx = 1\n")
	// 	bstr += fmt.Sprintf("\t%[1]s = C.PyMethod_Function(%[1]s)\n", methvar)
	// 	bstr += fmt.Sprintf("} else {\n")
	// 	bstr += fmt.Sprintf("\t%s = C.PyTuple_New(%d)\n", varnm, sz)
	// 	bstr += fmt.Sprintf("}\n")
	// }

	// TODO: more efficient to use strings.Builder here..
	bstr := fmt.Sprintf("%s := C.PyTuple_New(%d)\n", varnm, sz)
	for i := 0; i < sz; i++ {
		v := tuple.At(i)
		typ := v.Type()
		anm := pySafeArg(v.Name(), i)
		vsym := sym.symtype(typ)
		if vsym == nil {
			err := sym.addType(v, typ)
			if err != nil {
				return "", err
			}
			vsym = sym.symtype(typ)
			if vsym == nil {
				return "", fmt.Errorf("buildTuple: type still not found: %s", typ.String())
			}
		}
		// bstr += fmt.Sprintf("_pyargidx = C.long(_pyargstidx + %d)\n", i)

		bt, isb := typ.Underlying().(*types.Basic)
		switch {
		case vsym.goname == "interface{}":
			bstr += fmt.Sprintf("C.PyTuple_SetItem(%s, %d, C.gopy_build_string(%s(%s)%s))\n", varnm, i, vsym.go2py, anm, vsym.go2pyParenEx)
		case vsym.hasHandle(): // note: assuming int64 handles
			bstr += fmt.Sprintf("C.PyTuple_SetItem(%s, %d, C.gopy_build_int64(C.int64_t(%s(%s)%s)))\n", varnm, i, vsym.go2py, anm, vsym.go2pyParenEx)
		case isb:
			bk := bt.Kind()
			switch {
			case types.Int <= bk && bk <= types.Int64:
				bstr += fmt.Sprintf("C.PyTuple_SetItem(%s, %d, C.gopy_build_int64(C.int64_t(%s)))\n", varnm, i, anm)
			case types.Uint <= bk && bk <= types.Uintptr:
				bstr += fmt.Sprintf("C.PyTuple_SetItem(%s, %d, C.gopy_build_uint64(C.uint64_t(%s)))\n", varnm, i, anm)
			case types.Float32 <= bk && bk <= types.Float64:
				bstr += fmt.Sprintf("C.PyTuple_SetItem(%s, %d, C.gopy_build_float64(C.double(%s)))\n", varnm, i, anm)
			case bk == types.String:
				bstr += fmt.Sprintf("C.PyTuple_SetItem(%s, %d, C.gopy_build_string(C.CString(%s)))\n", varnm, i, anm)
			case bk == types.Bool:
				bstr += fmt.Sprintf("C.PyTuple_SetItem(%s, %d, C.gopy_build_bool(C.uint8_t(boolGoToPy(%s))))\n", varnm, i, anm)
			}
		default:
			return "", fmt.Errorf("buildTuple: type not handled: %s", typ.String())
		}
	}
	return bstr, nil
}

// pyObjectToGo returns code that decodes a PyObject variable of name objnm into a basic go type
func (sym *symtab) pyObjectToGo(typ types.Type, sy *symbol, objnm string) (string, error) {
	bstr := ""
	bt, isb := typ.Underlying().(*types.Basic)
	switch {
	// case vsym.goname == "interface{}":
	// 	bstr += fmt.Sprintf("C.PyTuple_SetItem(%s, %d, C.gopy_build_string(%s(%s)%s))\n", varnm, i, vsym.go2py, anm, vsym.go2pyParenEx)
	// case vsym.hasHandle(): // note: assuming int64 handles
	// 	bstr += fmt.Sprintf("C.PyTuple_SetItem(%s, %d, C.gopy_build_int64(C.int64_t(%s(%s)%s)))\n", varnm, i, vsym.go2py, anm, vsym.go2pyParenEx)
	case isb:
		bk := bt.Kind()
		switch {
		case types.Int <= bk && bk <= types.Int64:
			bstr += fmt.Sprintf("%s(C.PyLong_AsLongLong(%s))", sy.goname, objnm)
		case types.Uint <= bk && bk <= types.Uintptr:
			bstr += fmt.Sprintf("%s(C.PyLong_AsUnsignedLongLong(%s))", sy.goname, objnm)
		case types.Float32 <= bk && bk <= types.Float64:
			bstr += fmt.Sprintf("%s(C.PyFloat_AsDouble(%s))", sy.goname, objnm)
		case bk == types.String:
			bstr += fmt.Sprintf("C.GoString(C.PyBytes_AsString(%s))", objnm)
		case bk == types.Bool:
			bstr += fmt.Sprintf("boolPyToGo(C.char(C.PyLong_AsLongLong(%s)))", objnm)
		}
	default:
		return "", fmt.Errorf("pyObjectToGo: type not handled: %s", typ.String())
	}
	return bstr, nil
}

// ZeroToGo returns code for a zero of the given type, e.g., to synthesize a zero return value
func (sym *symtab) ZeroToGo(typ types.Type, sy *symbol) (string, error) {
	bstr := ""
	bt, isb := typ.Underlying().(*types.Basic)
	switch {
	// case vsym.goname == "interface{}":
	// 	bstr += fmt.Sprintf("C.PyTuple_SetItem(%s, %d, C.gopy_build_string(%s(%s)%s))\n", varnm, i, vsym.go2py, anm, vsym.go2pyParenEx)
	// case vsym.hasHandle(): // note: assuming int64 handles
	// 	bstr += fmt.Sprintf("C.PyTuple_SetItem(%s, %d, C.gopy_build_int64(C.int64_t(%s(%s)%s)))\n", varnm, i, vsym.go2py, anm, vsym.go2pyParenEx)
	case isb:
		bk := bt.Kind()
		switch {
		case types.Int <= bk && bk <= types.Float64:
			bstr += fmt.Sprintf("%s(0)%s", sy.py2go, sy.py2goParenEx)
		case bk == types.String:
			bstr += `C.GoString(nil)`
		case bk == types.Bool:
			bstr += fmt.Sprintf("false")
		}
	default:
		return "", fmt.Errorf("ZeroToGo: type not handled: %s", typ.String())
	}
	return bstr, nil
}

// typeNamePkg gets the goname and package for a given types.Type -- deals with
// naming for types in other packages, and adds those packages to imports paths.
// Falls back on sym.pkg if no other package info avail.
func (sym *symtab) typeNamePkg(t types.Type) (gonm, idnm string, pkg *types.Package) {
	pkg = sym.typePkg(t)
	if pkg != nil {
		sym.addImport(pkg)
	} else {
		pkg = sym.pkg
	}
	gonm = sym.typeGoName(t)
	idnm = sym.typeIdName(t)
	return
}

// addTypeIfNew adds given type if it is not already in the symbol table
// returns the symbol for the type or error if cannot be added
func (sym *symtab) addTypeIfNew(t types.Type) (*symbol, error) {
	fn := sym.fullTypeString(t)
	tsym := sym.sym(fn)
	if tsym != nil && tsym.goname != "" {
		return tsym, nil
	}
	tname, _, _ := sym.typeNamePkg(t)
	tobj := sym.pkg.Scope().Lookup(tname)
	if tobj != nil {
		sym.addSymbol(tobj)
	} else {
		if ntyp, ok := t.(*types.Named); ok {
			sym.addType(ntyp.Obj(), t)
		} else {
			sym.addType(nil, t)
		}
	}
	tsym = sym.sym(fn)
	if tsym == nil {
		return nil, fmt.Errorf("gopy: could not add new type: %q", tname)
	}
	return tsym, nil
}

func (sym *symtab) addType(obj types.Object, t types.Type) error {
	fn := sym.fullTypeString(t)
	n, id, pkg := sym.typeNamePkg(t)
	kind := skType
	switch typ := t.(type) {
	case *types.Basic:
		kind |= skBasic
		styp := sym.symtype(typ)
		if styp == nil {
			return fmt.Errorf("builtin type not already known [%s]!", n)
		}

	case *types.Pointer:
		return sym.addPointerType(pkg, obj, t, kind, id, n)

	case *types.Array:
		return sym.addArrayType(pkg, obj, t, kind, id, n)

	case *types.Slice:
		return sym.addSliceType(pkg, obj, t, kind, id, n)

	case *types.Map:
		return sym.addMapType(pkg, obj, t, kind, id, n)

	case *types.Signature:
		return sym.addSignatureType(pkg, obj, t, kind, id, n)

	case *types.Interface:
		return sym.addInterfaceType(pkg, obj, t, kind, id, n)

	case *types.Chan:
		return fmt.Errorf("gopy: channel type not supported: %s\n", n)

	case *types.Named:
		if !typ.Obj().Exported() {
			return fmt.Errorf("gopy: non-exported named type: %s\n", n)
		}
		kind |= skNamed
		var err error
		switch st := typ.Underlying().(type) {
		case *types.Struct:
			err = sym.addStructType(pkg, obj, t, kind, id, n)

		case *types.Basic:
			styp := sym.symtype(st)
			if styp == nil {
				return fmt.Errorf("gopy: type not found: %s\n", n)
			}
			py2go := sym.typeGoName(t)
			py2goParEx := ""
			if styp.py2go != "" {
				py2go += "(" + styp.py2go
				py2goParEx = ")"
			}
			go2py := styp.goname
			go2pyParEx := ""
			if styp.go2py != "" {
				go2py = styp.go2py + "(" + go2py
				go2pyParEx = ")"
			}

			sym.syms[fn] = &symbol{
				gopkg:        pkg,
				goobj:        obj,
				gotyp:        t,
				kind:         kind | skBasic,
				id:           id,
				goname:       styp.goname,
				cgoname:      styp.cgoname,
				cpyname:      styp.cpyname,
				pysig:        styp.pysig,
				go2py:        go2py,
				go2pyParenEx: go2pyParEx,
				py2go:        py2go,
				py2goParenEx: py2goParEx,
				zval:         styp.zval,
			}

		case *types.Array:
			err = sym.addArrayType(pkg, obj, t, kind, id, n)

		case *types.Slice:
			err = sym.addSliceType(pkg, obj, t, kind, id, n)

		case *types.Map:
			err = sym.addMapType(pkg, obj, t, kind, id, n)

		case *types.Signature:
			err = sym.addSignatureType(pkg, obj, t, kind, id, n)

		case *types.Pointer:
			err = sym.addPointerType(pkg, obj, t, kind, id, n)

		case *types.Interface:
			err = sym.addInterfaceType(pkg, obj, t, kind, id, n)

		case *types.Chan:
			err = fmt.Errorf("gopy: channel type not supported: %s", n)

		default:
			err = fmt.Errorf("unhandled named-type: [%T]\n%#v\n", obj, t)
		}

		if err != nil {
			return err
		}

		// add methods
		for i := 0; i < typ.NumMethods(); i++ {
			m := typ.Method(i)
			if !m.Exported() {
				continue
			}
			mid := id + "_" + m.Name()
			mname := m.Name()
			sym.addMethod(pkg, m, m.Type(), skFunc, mid, mname)
		}

	default:
		return fmt.Errorf("unhandled obj [%T]\ntype [%#v]", obj, t)
	}
	return nil
}

func (sym *symtab) addArrayType(pkg *types.Package, obj types.Object, t types.Type, kind symkind, id, n string) error {
	fn := sym.fullTypeString(t)
	typ := t.Underlying().(*types.Array)
	kind |= skArray
	elt := typ.Elem()
	elsym, err := sym.addTypeIfNew(elt)
	if err != nil {
		return err
	}
	if elsym.isSignature() {
		return fmt.Errorf("gopy: array value type cannot be signature / func: %q", elsym.goname)
	}
	sym.syms[fn] = &symbol{
		gopkg:   pkg,
		goobj:   obj,
		gotyp:   t,
		kind:    kind,
		id:      id,
		goname:  n,
		cgoname: "CGoHandle", // handles
		cpyname: PyHandle,
		pysig:   "[]" + elsym.pysig,
		go2py:   "handleFromPtr_" + id,
		py2go:   "deptrFromHandle_" + id,
		zval:    "nil",
	}
	return nil
}

func (sym *symtab) addMapType(pkg *types.Package, obj types.Object, t types.Type, kind symkind, id, n string) error {
	fn := sym.fullTypeString(t)
	typ := t.Underlying().(*types.Map)
	kind |= skMap
	elt := typ.Elem()
	elsym, err := sym.addTypeIfNew(elt)
	if err != nil {
		return err
	}
	if elsym.isSignature() {
		return fmt.Errorf("gopy: map value type cannot be signature / func: %q", elsym.goname)
	}
	// add type for keys method
	keyt := typ.Key()
	keyslt := types.NewSlice(keyt)
	_, err = sym.addTypeIfNew(keyslt)
	if err != nil {
		return err
	}
	sym.syms[fn] = &symbol{
		gopkg:   pkg,
		goobj:   obj,
		gotyp:   t,
		kind:    kind,
		id:      id,
		goname:  n,
		cgoname: "CGoHandle",
		cpyname: PyHandle,
		pysig:   "object",
		go2py:   "handleFromPtr_" + id,
		py2go:   "deptrFromHandle_" + id,
		zval:    "nil",
	}
	return nil
}

func (sym *symtab) addSliceType(pkg *types.Package, obj types.Object, t types.Type, kind symkind, id, n string) error {
	fn := sym.fullTypeString(t)
	typ := t.Underlying().(*types.Slice)
	kind |= skSlice
	elt := typ.Elem()
	elsym, err := sym.addTypeIfNew(elt)
	if err != nil {
		return err
	}
	if elsym.isSignature() {
		return fmt.Errorf("gopy: slice value type cannot be signature / func: %q", elsym.goname)
	}
	sym.syms[fn] = &symbol{
		gopkg:   pkg,
		goobj:   obj,
		gotyp:   t,
		kind:    kind,
		id:      id,
		goname:  n,
		cgoname: "CGoHandle",
		cpyname: PyHandle,
		pysig:   "[]" + elsym.pysig,
		go2py:   "handleFromPtr_" + id,
		py2go:   "deptrFromHandle_" + id,
		zval:    "nil",
	}
	return nil
}

func (sym *symtab) addStructType(pkg *types.Package, obj types.Object, t types.Type, kind symkind, id, n string) error {
	fn := sym.fullTypeString(t)
	typ := t.Underlying().(*types.Struct)
	kind |= skStruct
	// add our type first before adding fields -- prevents loops!
	sym.syms[fn] = &symbol{
		gopkg:   pkg,
		goobj:   obj,
		gotyp:   t,
		kind:    kind,
		id:      id,
		goname:  n,
		cgoname: "CGoHandle",
		cpyname: PyHandle,
		pysig:   "object",
		go2py:   "handleFromPtr_" + id,
		py2go:   "*ptrFromHandle_" + id,
		zval:    "nil",
	}
	for i := 0; i < typ.NumFields(); i++ {
		if isPrivate(typ.Field(i).Name()) {
			continue
		}
		f := typ.Field(i)
		if !f.Exported() || f.Embedded() {
			continue
		}
		ftyp := f.Type()
		fsym := sym.symtype(ftyp)
		if fsym == nil {
			err := sym.addType(f, ftyp)
			if err != nil {
				continue
			}
			fsym = sym.symtype(ftyp)
			if fsym == nil {
				continue
			}
		}
	}
	return nil
}

func (sym *symtab) addSignatureType(pkg *types.Package, obj types.Object, t types.Type, kind symkind, id, n string) error {
	fn := sym.fullTypeString(t)
	kind |= skSignature

	sig := t.Underlying().(*types.Signature)
	args := sig.Params()
	nargs := args.Len()
	rets := sig.Results()
	nsig := sym.typeGoName(t.Underlying())

	if rets.Len() > 1 {
		return fmt.Errorf("multiple return values not supported")
	}
	retstr := ""
	var ret *types.Var
	var rsym *symbol

	if rets.Len() == 1 {
		retstr = "_fcret := "
		ret = rets.At(0)
		rsym = sym.symtype(ret.Type())
		if rsym == nil {
			return fmt.Errorf("return type not supported: %s", n)
		}
	}

	if nargs > 0 { // need to deal with unnamed args
		nsig = "func ("
		for i := 0; i < nargs; i++ {
			v := args.At(i)
			typ := v.Type()
			anm := pySafeArg(v.Name(), i)
			if i > 0 {
				nsig += ", "
			}
			nsig += anm + " " + sym.typeGoName(typ)
		}
		nsig += ")"
		if rets.Len() == 1 {
			nsig += " " + sym.typeGoName(ret.Type())
		}
	}

	py2g := fmt.Sprintf("%s { ", nsig)

	py2g += "_gstate := C.PyGILState_Ensure()\n"

	// TODO: use strings.Builder
	if rets.Len() == 0 {
		py2g += "if C.PyCallable_Check(_fun_arg) == 0 {\n"
		py2g += "C.PyGILState_Release(_gstate)\n" // Release GIL
		py2g += "return\n"
		py2g += "}\n"
	} else {
		zstr, err := sym.ZeroToGo(ret.Type(), rsym)
		if err != nil {
			return err
		}
		py2g += "if C.PyCallable_Check(_fun_arg) == 0 {\n"
		py2g += "C.PyGILState_Release(_gstate)\n" // Release GIL
		py2g += fmt.Sprintf("return %s\n", zstr)
		py2g += "}\n"
	}

	if nargs > 0 {
		bstr, err := sym.buildTuple(args, "_fcargs", "_fun_arg")
		if err != nil {
			return err
		}
		py2g += bstr + retstr
		py2g += "C.PyObject_CallObject(_fun_arg, _fcargs)\n"
		py2g += "C.gopy_decref(_fcargs)\n"
	} else {
		// TODO: methods not supported for no-args case -- requires self arg..
		py2g += retstr + "C.PyObject_CallObject(_fun_arg, nil)\n"
	}
	py2g += "C.gopy_err_handle()\n"
	py2g += "C.PyGILState_Release(_gstate)\n"
	if rets.Len() == 1 {
		cvt, err := sym.pyObjectToGo(ret.Type(), rsym, "_fcret")
		if err != nil {
			return err
		}
		py2g += fmt.Sprintf("return %s", cvt)
	}
	py2g += "}"

	sym.syms[fn] = &symbol{
		gopkg:   pkg,
		goobj:   obj,
		gotyp:   t,
		kind:    kind,
		id:      id,
		goname:  n,
		cgoname: "*C.PyObject",
		cpyname: "PyObject*",
		pysig:   "callable",
		go2py:   "?",
		py2go:   py2g,
	}
	return nil
}

func (sym *symtab) addMethod(pkg *types.Package, obj types.Object, t types.Type, kind symkind, id, n string) error {
	sig := t.Underlying().(*types.Signature)
	_, _, _, err := isPyCompatFunc(sig)
	if err != nil {
		if !NoWarn {
			fmt.Printf("ignoring python incompatible method: %v.%v: %v: %v\n", pkg.Name(), obj.String(), t.String(), err)
		}
	}
	if err == nil {
		fn := types.ObjectString(obj, nil)
		kind |= skFunc
		sym.syms[fn] = &symbol{
			gopkg:   pkg,
			goobj:   obj,
			gotyp:   t,
			kind:    kind,
			id:      id,
			goname:  n,
			cgoname: fn + "_" + id,
			cpyname: fn + "_" + id,
		}
		sym.processTuple(sig.Results())
		sym.processTuple(sig.Params())
	}
	return nil
}

func (sym *symtab) addPointerType(pkg *types.Package, obj types.Object, t types.Type, kind symkind, id, n string) error {
	fn := sym.fullTypeString(t)
	typ := t.Underlying().(*types.Pointer)
	etyp := typ.Elem()
	esym := sym.symtype(etyp)
	if esym == nil {
		sym.addType(obj, etyp)
		esym = sym.symtype(etyp)
		if esym == nil {
			return fmt.Errorf("gopy: could not retrieve symbol for %q", sym.fullTypeString(etyp))
		}
	}

	sym.syms[fn] = &symbol{
		gopkg:   pkg,
		goobj:   obj,
		gotyp:   t,
		kind:    esym.kind | skPointer,
		id:      id,
		goname:  n,
		cgoname: "CGoHandle", // handles
		cpyname: PyHandle,
		pysig:   "object",
		go2py:   "handleFromPtr_" + id,
		py2go:   "ptrFromHandle_" + id,
		zval:    "nil",
	}
	return nil
}

func (sym *symtab) addInterfaceType(pkg *types.Package, obj types.Object, t types.Type, kind symkind, id, n string) error {
	fn := sym.fullTypeString(t)
	typ := t.Underlying().(*types.Interface)
	kind |= skInterface
	// special handling of 'error'
	if isErrorType(typ) {
		return nil
	}

	if fn == "interface{}" {
		sym.syms[fn] = &symbol{
			gopkg:        pkg,
			goobj:        obj,
			gotyp:        t,
			kind:         kind,
			id:           id,
			goname:       n,
			cgoname:      "*C.char",
			cpyname:      "char*",
			pysig:        "str",
			go2py:        `C.CString(fmt.Sprintf("%s",`,
			go2pyParenEx: "))",
			py2go:        "C.GoString",
			zval:         `""`,
		}
	} else {
		sym.syms[fn] = &symbol{
			gopkg:   pkg,
			goobj:   obj,
			gotyp:   t,
			kind:    kind,
			id:      id,
			goname:  n,
			cgoname: "CGoHandle",
			cpyname: PyHandle,
			pysig:   "object",
			go2py:   "handleFromPtr_" + id,
			py2go:   "ptrFromHandle_" + id,
			zval:    "nil",
		}
	}
	return nil
}

func (sym *symtab) print() {
	fmt.Printf("\n\n%s\n", strings.Repeat("=", 80))
	for _, n := range sym.names() {
		fmt.Printf("%q (kind=%v)\n", n, sym.syms[n].kind)
	}
	fmt.Printf("%s\n", strings.Repeat("=", 80))
}

func init() {

	universe = newSymtab(nil, nil)
	universe.parent = nil

	universe.syms = stdBasicTypes()

	addStdSliceMaps()

	current = newSymtab(nil, universe)
}
