package taskengine

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// ErrChainLint marks every defect the load-time chain linter reports, so
// services can distinguish "this chain is invalid" (disable it, teach the
// author) from I/O failures (retry, propagate).
var ErrChainLint = errors.New("chain failed load-time validation")

type dtSet uint16

func dtBit(d DataType) dtSet { return 1 << uint(d) }

func (s dtSet) has(d DataType) bool     { return s&dtBit(d) != 0 }
func (s dtSet) intersects(o dtSet) bool { return s&o != 0 }
func (s dtSet) union(o dtSet) dtSet     { return s | o }
func (s dtSet) isEmpty() bool           { return s == 0 }

var lintOrderedDataTypes = []DataType{
	DataTypeString, DataTypeInt, DataTypeJSON, DataTypeChatHistory, DataTypeNil, DataTypeAny,
}

func (s dtSet) describe() string {
	if s.isEmpty() {
		return "nothing"
	}
	names := make([]string, 0, len(lintOrderedDataTypes))
	for _, d := range lintOrderedDataTypes {
		if s.has(d) {
			dt := d
			names = append(names, dt.String())
		}
	}
	return strings.Join(names, ", ")
}

func acceptSet(sig HandlerSignature) dtSet {
	if len(sig.Inputs) == 0 {
		var all dtSet
		for _, d := range lintOrderedDataTypes {
			all = all.union(dtBit(d))
		}
		return all
	}
	var s dtSet
	for _, d := range sig.Inputs {
		s = s.union(dtBit(d))
	}
	return s
}

// LintChain vets a chain at load time; entryTypes are the DataTypes the caller may
// feed the entry task (omitted treated as DataTypeAny), and all defects are collected
// and joined into one error wrapping ErrChainLint.
func LintChain(chain *TaskChainDefinition, entryTypes ...DataType) error {
	if chain == nil {
		return fmt.Errorf("%w: chain is nil", ErrChainLint)
	}
	// Structural checks first: the dataflow walk assumes unique IDs, known handlers,
	// and resolvable goto/on_failure targets.
	if err := validateChain(chain.Tasks); err != nil {
		return fmt.Errorf("%w: chain[%s]: %w", ErrChainLint, chain.ID, err)
	}

	entry := dtBit(DataTypeAny)
	if len(entryTypes) > 0 {
		entry = 0
		for _, d := range entryTypes {
			entry = entry.union(dtBit(d))
		}
	}

	l := newChainLinter(chain, entry)
	l.propagate()
	errs := l.check()
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("%w: chain[%s]:\n%w", ErrChainLint, chain.ID, errors.Join(errs...))
}

type chainLinter struct {
	chain *TaskChainDefinition
	tasks map[string]*TaskDefinition
	order []string

	entrySet dtSet

	inSets    map[string]dtSet
	varSets   map[string]dtSet
	avail     map[string]map[string]bool
	reachable map[string]bool
}

func newChainLinter(chain *TaskChainDefinition, entry dtSet) *chainLinter {
	l := &chainLinter{
		chain:     chain,
		tasks:     make(map[string]*TaskDefinition, len(chain.Tasks)),
		order:     make([]string, 0, len(chain.Tasks)),
		entrySet:  entry,
		inSets:    map[string]dtSet{},
		varSets:   map[string]dtSet{"input": entry},
		avail:     map[string]map[string]bool{},
		reachable: map[string]bool{},
	}
	for i := range chain.Tasks {
		t := &chain.Tasks[i]
		l.tasks[t.ID] = t
		l.order = append(l.order, t.ID)
		l.avail[t.ID] = map[string]bool{}
	}
	entryID := chain.Tasks[0].ID
	l.reachable[entryID] = true
	l.inSets[entryID] = entry
	l.avail[entryID]["input"] = true
	return l
}

func (l *chainLinter) effectiveInput(t *TaskDefinition) dtSet {
	if strings.TrimSpace(t.PromptTemplate) != "" {
		return dtBit(DataTypeString)
	}
	if t.InputVar != "" {
		return l.varSets[t.InputVar]
	}
	return l.inSets[t.ID]
}

func (l *chainLinter) successOut(t *TaskDefinition) dtSet {
	sig := handlerSignatures[t.Handler]
	switch sig.Mode {
	case HandlerOutputFixed:
		return dtBit(sig.Output)
	case HandlerOutputPassthrough:
		return l.effectiveInput(t)
	case HandlerOutputDynamic:
		if strings.TrimSpace(t.OutputTemplate) != "" {
			return dtBit(DataTypeString)
		}
		return dtBit(DataTypeAny)
	case HandlerOutputNone:
		return 0
	}
	return dtBit(DataTypeAny)
}

func (l *chainLinter) failureOut(t *TaskDefinition) dtSet {
	return l.effectiveInput(t).union(l.successOut(t))
}

func successTargets(t *TaskDefinition) []string {
	seen := map[string]bool{}
	var out []string
	for _, br := range t.Transition.Branches {
		g := br.Goto
		if g == "" || g == TermEnd || seen[g] {
			continue
		}
		seen[g] = true
		out = append(out, g)
	}
	return out
}

func (l *chainLinter) propagate() {
	for changed := true; changed; {
		changed = false
		for _, id := range l.order {
			t := l.tasks[id]
			if !l.reachable[id] {
				continue
			}
			sOut := l.successOut(t)
			fOut := l.failureOut(t)

			if l.growVar(t.ID, sOut) {
				changed = true
			}
			if l.growVar("previous_output", sOut) {
				changed = true
			}
			if f := t.Transition.OnFailure; f != "" {
				if l.growVar(t.ID, fOut) {
					changed = true
				}
				if l.growVar("previous_output", fOut) {
					changed = true
				}
				if l.growVar("last_error", dtBit(DataTypeString)) {
					changed = true
				}
				if l.growVar(t.ID+"_error", dtBit(DataTypeString)) {
					changed = true
				}
			}

			if !sOut.isEmpty() {
				for _, g := range successTargets(t) {
					if l.growIn(g, sOut) {
						changed = true
					}
					if l.growAvail(g, l.avail[id], t.ID, "previous_output") {
						changed = true
					}
					if !l.reachable[g] {
						l.reachable[g] = true
						changed = true
					}
				}
			}
			if f := t.Transition.OnFailure; f != "" {
				if l.growIn(f, fOut) {
					changed = true
				}
				if l.growAvail(f, l.avail[id], t.ID, "previous_output", "last_error", t.ID+"_error") {
					changed = true
				}
				if !l.reachable[f] {
					l.reachable[f] = true
					changed = true
				}
			}
		}
	}
}

func (l *chainLinter) growVar(name string, s dtSet) bool {
	old := l.varSets[name]
	merged := old.union(s)
	if merged != old {
		l.varSets[name] = merged
		return true
	}
	return false
}

func (l *chainLinter) growIn(id string, s dtSet) bool {
	old := l.inSets[id]
	merged := old.union(s)
	if merged != old {
		l.inSets[id] = merged
		return true
	}
	return false
}

func (l *chainLinter) growAvail(id string, from map[string]bool, extra ...string) bool {
	dst := l.avail[id]
	grew := false
	for name := range from {
		if !dst[name] {
			dst[name] = true
			grew = true
		}
	}
	for _, name := range extra {
		if !dst[name] {
			dst[name] = true
			grew = true
		}
	}
	return grew
}

func (l *chainLinter) check() []error {
	var errs []error
	for _, id := range l.order {
		t := l.tasks[id]
		sig := handlerSignatures[t.Handler]

		errs = append(errs, l.checkRouteVocabulary(t)...)
		errs = append(errs, l.checkMacros(t)...)
		errs = append(errs, l.checkTemplateRefs(t)...)
		errs = append(errs, l.checkDeadBranches(t, sig)...)

		if !l.reachable[id] {
			continue
		}
		errs = append(errs, l.checkInputVar(t)...)
		errs = append(errs, l.checkInputTypes(t, sig)...)
	}
	return errs
}

func (l *chainLinter) checkRouteVocabulary(t *TaskDefinition) []error {
	if t.Handler != HandleRoute {
		return nil
	}
	for _, br := range t.Transition.Branches {
		if br.Operator == OpEquals && strings.TrimSpace(br.When) != "" {
			return nil
		}
	}
	return []error{fmt.Errorf("task[%s]: a route task needs at least one equals branch — the branch labels are the answer vocabulary the model routes between", t.ID)}
}

func (l *chainLinter) checkInputVar(t *TaskDefinition) []error {
	v := t.InputVar
	if v == "" || v == "input" || l.avail[t.ID][v] {
		return nil
	}
	known := make([]string, 0, len(l.avail[t.ID]))
	for name := range l.avail[t.ID] {
		known = append(known, name)
	}
	sort.Strings(known)
	return []error{fmt.Errorf("task[%s]: input_var %q is never produced by any task that can run before it (variables available here: %s)",
		t.ID, v, strings.Join(known, ", "))}
}

func (l *chainLinter) checkInputTypes(t *TaskDefinition, sig HandlerSignature) []error {
	accepts := acceptSet(sig)
	acceptsAny := len(sig.Inputs) == 0

	if strings.TrimSpace(t.PromptTemplate) != "" {
		if !acceptsAny && !accepts.has(DataTypeString) {
			return []error{fmt.Errorf("task[%s] handler %s cannot take a prompt_template: a rendered template is a string, and %s accepts %s",
				t.ID, t.Handler, t.Handler, sig.acceptsDescription())}
		}
		return nil
	}

	if t.InputVar != "" {
		s := l.varSets[t.InputVar]
		if s.isEmpty() || s.has(DataTypeAny) || acceptsAny {
			return nil
		}
		if !s.intersects(accepts) {
			return []error{fmt.Errorf("task[%s] handler %s cannot accept input from input_var %q (produces %s; accepts %s)",
				t.ID, t.Handler, t.InputVar, s.describe(), sig.acceptsDescription())}
		}
		return nil
	}

	if acceptsAny {
		return nil
	}

	var errs []error
	entryID := l.chain.Tasks[0].ID
	if t.ID == entryID {
		if !l.entrySet.has(DataTypeAny) && !l.entrySet.intersects(accepts) {
			errs = append(errs, fmt.Errorf("task[%s] handler %s cannot accept the chain input (chain input is %s; accepts %s)",
				t.ID, t.Handler, l.entrySet.describe(), sig.acceptsDescription()))
		}
	}
	for _, pid := range l.order {
		p := l.tasks[pid]
		if !l.reachable[pid] {
			continue
		}
		if edgeErr := l.checkEdge(p, t, sig, accepts); edgeErr != nil {
			errs = append(errs, edgeErr)
		}
	}
	return errs
}

func (l *chainLinter) checkEdge(p, t *TaskDefinition, sig HandlerSignature, accepts dtSet) error {
	viaSuccess := false
	for _, g := range successTargets(p) {
		if g == t.ID {
			viaSuccess = true
			break
		}
	}
	viaFailure := p.Transition.OnFailure == t.ID

	var carried dtSet
	if viaSuccess {
		carried = carried.union(l.successOut(p))
	}
	if viaFailure {
		carried = carried.union(l.failureOut(p))
	}
	if carried.isEmpty() || carried.has(DataTypeAny) {
		return nil // no live edge, or unknown types
	}
	if carried.intersects(accepts) {
		return nil // at least one runtime value can pass
	}
	return fmt.Errorf("task[%s] handler %s cannot accept input from task[%s] (produces %s; accepts %s)",
		t.ID, t.Handler, p.ID, carried.describe(), sig.acceptsDescription())
}

func (l *chainLinter) checkDeadBranches(t *TaskDefinition, sig HandlerSignature) []error {
	if sig.SuccessEvals == nil {
		return nil
	}
	var errs []error
	for _, br := range t.Transition.Branches {
		var matches func(tok string) bool
		switch br.Operator {
		case OpEquals:
			matches = func(tok string) bool { return tok == br.When }
		case OpContains:
			matches = func(tok string) bool { return strings.Contains(tok, br.When) }
		case OpStartsWith:
			matches = func(tok string) bool { return strings.HasPrefix(tok, br.When) }
		case OpEndsWith:
			matches = func(tok string) bool { return strings.HasSuffix(tok, br.When) }
		default:
			continue
		}
		dead := true
		for _, tok := range sig.SuccessEvals {
			if matches(tok) {
				dead = false
				break
			}
		}
		if dead {
			errs = append(errs, fmt.Errorf("task[%s]: branch (%s %q) can never fire: handler %s only evaluates transitions against %s — branch on one of those control tokens, or use the route handler to branch on model text",
				t.ID, br.Operator, br.When, t.Handler, strings.Join(sig.SuccessEvals, ", ")))
		}
	}
	return errs
}

var lintVarMacroRe = regexp.MustCompile(`\{\{var:([^}]*)\}\}`)

func (l *chainLinter) checkMacros(t *TaskDefinition) []error {
	var errs []error
	fields := []struct{ name, text string }{
		{"system_instruction", t.SystemInstruction},
		{"prompt_template", t.PromptTemplate},
		{"output_template", t.OutputTemplate},
		{"print", t.Print},
	}
	for _, f := range fields {
		if f.text == "" {
			continue
		}
		for _, m := range stepMacroEdgeCountRe.FindAllStringSubmatch(f.text, -1) {
			edge := strings.TrimSpace(m[1])
			from, to, ok := strings.Cut(edge, "->")
			if !ok || from == "" || to == "" {
				errs = append(errs, fmt.Errorf("task[%s]: %s macro {{edge_count:%s}} is malformed — the edge form is fromTaskID->toTaskID", t.ID, f.name, edge))
				continue
			}
			if _, exists := l.tasks[from]; !exists {
				errs = append(errs, fmt.Errorf("task[%s]: %s macro {{edge_count:%s}} references unknown task %q", t.ID, f.name, edge, from))
			}
			if to == TermEnd {
				errs = append(errs, fmt.Errorf("task[%s]: %s macro {{edge_count:%s}} counts an edge into 'end', which is never incremented (the chain stops before counting) — reference a task-to-task edge", t.ID, f.name, edge))
				continue
			}
			if _, exists := l.tasks[to]; !exists {
				errs = append(errs, fmt.Errorf("task[%s]: %s macro {{edge_count:%s}} references unknown task %q", t.ID, f.name, edge, to))
			}
		}
		for _, m := range lintVarMacroRe.FindAllStringSubmatch(f.text, -1) {
			if strings.TrimSpace(m[1]) == "" {
				errs = append(errs, fmt.Errorf("task[%s]: %s macro {{var:}} names no variable — write {{var:name}} or {{var:name|fallback}}", t.ID, f.name))
			}
		}
	}
	return errs
}

var lintTemplateRefRe = regexp.MustCompile(`\{\{-?\s*\.([A-Za-z_][A-Za-z0-9_]*)[^}]*\}\}`)

func (l *chainLinter) checkTemplateRefs(t *TaskDefinition) []error {
	known := func(name string) bool {
		switch name {
		case "input", "previous_output", "last_error":
			return true
		}
		if _, ok := l.tasks[name]; ok {
			return true
		}
		if base, found := strings.CutSuffix(name, "_error"); found {
			if _, ok := l.tasks[base]; ok {
				return true
			}
		}
		return false
	}
	var errs []error
	for _, f := range []struct{ name, text string }{
		{"prompt_template", t.PromptTemplate},
		{"print", t.Print},
	} {
		if f.text == "" {
			continue
		}
		for _, m := range lintTemplateRefRe.FindAllStringSubmatch(f.text, -1) {
			if !known(m[1]) {
				errs = append(errs, fmt.Errorf("task[%s]: %s references {{.%s}}, but no task or engine variable with that name exists — it would render as the literal \"<no value>\" (known: input, previous_output, last_error, any task id, any <task id>_error)",
					t.ID, f.name, m[1]))
			}
		}
	}
	return errs
}
