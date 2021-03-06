package eval

import (
	"fmt"
	"os"

	"github.com/xiaq/elvish/parse"
	"github.com/xiaq/elvish/util"
)

// Compiler compiles an Elvish AST into an Op.
type Compiler struct {
	compilerEphemeral
}

// compilerEphemeral wraps the ephemeral parts of a Compiler.
type compilerEphemeral struct {
	name, text string
	scopes     []map[string]Type
	enclosed   map[string]Type
}

type commandType int

const (
	commandBuiltinFunction commandType = iota
	commandBuiltinSpecial
	commandDefinedFunction
	commandClosure
	commandExternal
)

// commandResolution packs information known about a command.
type commandResolution struct {
	streamTypes    [2]StreamType
	commandType    commandType
	builtinFunc    *builtinFunc
	builtinSpecial *builtinSpecial
	specialOp      strOp
}

// NewCompiler returns a new compiler.
func NewCompiler() *Compiler {
	return &Compiler{}
}

func (cp *Compiler) startCompile(name, text string, scope map[string]Type) {
	cp.compilerEphemeral = compilerEphemeral{
		name, text, []map[string]Type{scope}, make(map[string]Type),
	}
}

// Compile compiles a ChunkNode into an Op, with the knowledge of current
// scope. The supplied name and text are used in diagnostic messages.
func (cp *Compiler) Compile(name, text string, n *parse.ChunkNode, scope map[string]Type) (op Op, err error) {
	cp.startCompile(name, text, scope)
	defer util.Recover(&err)
	return cp.compileChunk(n), nil
}

func (cp *Compiler) pushScope() {
	cp.scopes = append(cp.scopes, make(map[string]Type))
}

func (cp *Compiler) popScope() {
	cp.scopes[len(cp.scopes)-1] = nil
	cp.scopes = cp.scopes[:len(cp.scopes)-1]
}

func (cp *Compiler) pushVar(name string, t Type) {
	cp.scopes[len(cp.scopes)-1][name] = t
}

func (cp *Compiler) popVar(name string) {
	delete(cp.scopes[len(cp.scopes)-1], name)
}

func (cp *Compiler) hasVarOnThisScope(name string) bool {
	_, ok := cp.scopes[len(cp.scopes)-1][name]
	return ok
}

func (cp *Compiler) errorf(p parse.Pos, format string, args ...interface{}) {
	util.Panic(util.NewContextualError(cp.name, cp.text, int(p), format, args...))
}

// compileChunk compiles a ChunkNode into an Op.
func (cp *Compiler) compileChunk(cn *parse.ChunkNode) Op {
	ops := make([]valuesOp, len(cn.Nodes))
	for i, pn := range cn.Nodes {
		ops[i], _ = cp.compilePipeline(pn)
	}
	return combineChunk(ops)
}

// compileClosure compiles a ClosureNode into a valuesOp along with its capture
// and the external stream types it expects.
func (cp *Compiler) compileClosure(cn *parse.ClosureNode) (valuesOp, map[string]Type, [2]StreamType) {
	ops := make([]valuesOp, len(cn.Chunk.Nodes))

	cp.pushScope()

	bounds := [2]StreamType{}
	for i, pn := range cn.Chunk.Nodes {
		var b [2]StreamType
		ops[i], b = cp.compilePipeline(pn)

		var ok bool
		bounds[0], ok = bounds[0].commonType(b[0])
		if !ok {
			cp.errorf(pn.Pos, "Pipeline input stream incompatible with previous ones")
		}
		bounds[1], ok = bounds[1].commonType(b[1])
		if !ok {
			cp.errorf(pn.Pos, "Pipeline output stream incompatible with previous ones")
		}
	}

	enclosed := cp.enclosed
	cp.enclosed = make(map[string]Type)
	cp.popScope()

	return combineClosure(ops, enclosed, bounds), enclosed, bounds
}

// compilePipeline compiles a PipelineNode into a valuesOp along with the
// external stream types it expects.
func (cp *Compiler) compilePipeline(pn *parse.PipelineNode) (valuesOp, [2]StreamType) {
	ops := make([]stateUpdatesOp, len(pn.Nodes))
	var bounds [2]StreamType
	internals := make([]StreamType, len(pn.Nodes)-1)

	var lastOutput StreamType
	for i, fn := range pn.Nodes {
		var b [2]StreamType
		ops[i], b = cp.compileForm(fn)
		input := b[0]
		if i == 0 {
			bounds[0] = input
		} else {
			internal, ok := lastOutput.commonType(input)
			if !ok {
				cp.errorf(fn.Pos, "Form input type %v insatisfiable - previous form output is type %v", input, lastOutput)
			}
			internals[i-1] = internal
		}
		lastOutput = b[1]
	}
	bounds[1] = lastOutput
	return combinePipeline(ops, bounds, internals, pn.Pos), bounds
}

// mustResolveVar calls ResolveVar and calls errorf if the variable is
// nonexistent.
func (cp *Compiler) mustResolveVar(name string, p parse.Pos) Type {
	if t := cp.ResolveVar(name); t != nil {
		return t
	}
	cp.errorf(p, "undefined variable $%s", name)
	return nil
}

// ResolveVar returns the type of a variable with supplied name, found in
// current or upper scopes. If such a variable is nonexistent, a nil is
// returned.
func (cp *Compiler) ResolveVar(name string) Type {
	thisScope := len(cp.scopes) - 1
	for i := thisScope; i >= 0; i-- {
		if t := cp.scopes[i][name]; t != nil {
			if i < thisScope {
				cp.enclosed[name] = t
			}
			return t
		}
	}
	return nil
}

// resolveCommand tries to find a command with supplied name and modify the
// commandResolution in place.
func (cp *Compiler) resolveCommand(name string, cr *commandResolution) {
	if ct, ok := cp.ResolveVar("fn-" + name).(ClosureType); ok {
		// Defined function
		cr.commandType = commandDefinedFunction
		cr.streamTypes = ct.Bounds
	} else if bi, ok := builtinSpecials[name]; ok {
		// Builtin special
		cr.commandType = commandBuiltinSpecial
		cr.streamTypes = bi.streamTypes
		cr.builtinSpecial = &bi
	} else if bi, ok := builtinFuncs[name]; ok {
		// Builtin func
		cr.commandType = commandBuiltinFunction
		cr.streamTypes = bi.streamTypes
		cr.builtinFunc = &bi
	} else {
		// External command
		cr.commandType = commandExternal
		cr.streamTypes = [2]StreamType{fdStream, fdStream}
	}
}

// compileForm compiles a FormNode into a stateUpdatesOp along with the
// external stream types it expects.
func (cp *Compiler) compileForm(fn *parse.FormNode) (stateUpdatesOp, [2]StreamType) {
	// TODO(xiaq): Allow more interesting compound expressions to be used as
	// commands
	msg := "command must be a string or closure"
	if len(fn.Command.Nodes) != 1 || fn.Command.Nodes[0].Right != nil {
		cp.errorf(fn.Command.Pos, msg)
	}
	command := fn.Command.Nodes[0].Left
	cmdOp, pbounds := cp.compilePrimary(command)

	resolution := &commandResolution{}
	switch command.Typ {
	case parse.StringPrimary:
		cp.resolveCommand(command.Node.(*parse.StringNode).Text, resolution)
	case parse.ClosurePrimary:
		resolution.commandType = commandClosure
		resolution.streamTypes = *pbounds
	default:
		cp.errorf(fn.Command.Pos, msg)
	}

	var nports uintptr
	for _, rd := range fn.Redirs {
		if nports < rd.Fd()+1 {
			nports = rd.Fd() + 1
		}
	}

	ports := make([]portOp, nports)
	for _, rd := range fn.Redirs {
		fd := rd.Fd()
		if fd < 2 {
			switch rd := rd.(type) {
			case *parse.FdRedir:
				if resolution.streamTypes[fd] == chanStream {
					cp.errorf(rd.Pos, "fd redir on channel port")
				}
			case *parse.FilenameRedir:
				if resolution.streamTypes[fd] == chanStream {
					cp.errorf(rd.Pos, "filename redir on channel port")
				}
			}
			resolution.streamTypes[fd] = unusedStream
		}
		ports[fd] = cp.compileRedir(rd)
	}

	var tlist valuesOp
	if resolution.commandType == commandBuiltinSpecial {
		resolution.specialOp = resolution.builtinSpecial.compile(cp, fn)
	} else {
		tlist = cp.compileSpaced(fn.Args)
	}
	return combineForm(cmdOp, tlist, ports, resolution, fn.Pos), resolution.streamTypes
}

// compileRedir compiles a Redir into a portOp.
func (cp *Compiler) compileRedir(r parse.Redir) portOp {
	switch r := r.(type) {
	case *parse.CloseRedir:
		return func(ev *Evaluator) *port {
			return &port{}
		}
	case *parse.FdRedir:
		oldFd := int(r.OldFd)
		return func(ev *Evaluator) *port {
			// Copied ports have shouldClose unmarked to avoid double close on
			// channels
			p := *ev.port(oldFd)
			p.shouldClose = false
			return &p
		}
	case *parse.FilenameRedir:
		fnameOp := cp.compileCompound(r.Filename)
		return func(ev *Evaluator) *port {
			fname := string(*ev.mustSingleString(
				fnameOp.f(ev), "filename", r.Filename.Pos))
			// TODO haz hardcoded permbits now
			f, e := os.OpenFile(fname, r.Flag, 0644)
			if e != nil {
				ev.errorf(r.Pos, "failed to open file %q: %s", fname[0], e)
			}
			return &port{f: f, shouldClose: true}
		}
	default:
		panic("bad Redir type")
	}
}

// compileCompounds compiles a slice of CompoundNode's into a valuesOp. It can
// be also used to compile a SpacedNode.
func (cp *Compiler) compileCompounds(tns []*parse.CompoundNode) valuesOp {
	ops := make([]valuesOp, len(tns))
	for i, tn := range tns {
		ops[i] = cp.compileCompound(tn)
	}
	return combineSpaced(ops)
}

// compileSpaced compiles a SpacedNode into a valuesOp.
func (cp *Compiler) compileSpaced(ln *parse.SpacedNode) valuesOp {
	return cp.compileCompounds(ln.Nodes)
}

// compileCompound compiles a CompoundNode into a valuesOp.
func (cp *Compiler) compileCompound(tn *parse.CompoundNode) valuesOp {
	ops := make([]valuesOp, len(tn.Nodes))
	for i, fn := range tn.Nodes {
		ops[i], _ = cp.compileSubscript(fn)
	}
	op := combineCompound(ops)
	if tn.Sigil == parse.NoSigil {
		return op
	}
	cmd := string(tn.Sigil)
	cr := &commandResolution{}
	cp.resolveCommand(cmd, cr)
	fop := combineForm(makeString(cmd), op, nil, cr, tn.Pos)
	pop := combinePipeline([]stateUpdatesOp{fop}, cr.streamTypes, nil, tn.Pos)
	return combineOutputCapture(pop, cr.streamTypes)
}

// compileSubscript compiles a SubscriptNode into a valuesOp and if the
// subscript expression is a closure, the external stream types it expect.
func (cp *Compiler) compileSubscript(sn *parse.SubscriptNode) (valuesOp, *[2]StreamType) {
	if sn.Right == nil {
		return cp.compilePrimary(sn.Left)
	}
	left, _ := cp.compilePrimary(sn.Left)
	right := cp.compileCompound(sn.Right)
	return combineSubscript(cp, left, right, sn.Left.Pos, sn.Right.Pos), nil
}

// compilePrimary compiles a PrimaryNode into a valuesOp and if the primary
// expression is a closure, the external stream types it expect.
func (cp *Compiler) compilePrimary(fn *parse.PrimaryNode) (valuesOp, *[2]StreamType) {
	switch fn.Typ {
	case parse.StringPrimary:
		text := fn.Node.(*parse.StringNode).Text
		return makeString(text), nil
	case parse.VariablePrimary:
		name := fn.Node.(*parse.StringNode).Text
		return makeVar(cp, name, fn.Pos), nil
	case parse.TablePrimary:
		table := fn.Node.(*parse.TableNode)
		list := cp.compileCompounds(table.List)
		keys := make([]valuesOp, len(table.Dict))
		values := make([]valuesOp, len(table.Dict))
		for i, tp := range table.Dict {
			keys[i] = cp.compileCompound(tp.Key)
			values[i] = cp.compileCompound(tp.Value)
		}
		return combineTable(list, keys, values, fn.Pos), nil
	case parse.ClosurePrimary:
		op, enclosed, bounds := cp.compileClosure(fn.Node.(*parse.ClosureNode))
		for name, typ := range enclosed {
			if !cp.hasVarOnThisScope(name) {
				cp.enclosed[name] = typ
			}
		}
		return op, &bounds
	case parse.ListPrimary:
		return cp.compileSpaced(fn.Node.(*parse.SpacedNode)), nil
	case parse.OutputCapturePrimary:
		op, b := cp.compilePipeline(fn.Node.(*parse.PipelineNode))
		return combineOutputCapture(op, b), nil
	case parse.StatusCapturePrimary:
		op, _ := cp.compilePipeline(fn.Node.(*parse.PipelineNode))
		return op, nil
	default:
		panic(fmt.Sprintln("bad PrimaryNode type", fn.Typ))
	}
}
