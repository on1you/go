// Copyright 2012 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package build

import (
	"exp/locale/collate"
	"exp/norm"
	"fmt"
	"io"
	"log"
	"sort"
	"strings"
)

// TODO: optimizations:
// - expandElem is currently 20K. By putting unique colElems in a separate
//   table and having a byte array of indexes into this table, we can reduce
//   the total size to about 7K. By also factoring out the length bytes, we
//   can reduce this to about 6K.
// - trie valueBlocks are currently 100K. There are a lot of sparse blocks
//   and many consecutive values with the same stride. This can be further
//   compacted.

// entry is used to keep track of a single entry in the collation element table
// during building. Examples of entries can be found in the Default Unicode
// Collation Element Table.
// See http://www.unicode.org/Public/UCA/6.0.0/allkeys.txt.
type entry struct {
	runes []rune
	elems [][]int // the collation elements for runes
	str   string  // same as string(runes)

	decompose         bool // can use NFKD decomposition to generate elems
	expansionIndex    int  // used to store index into expansion table
	contractionHandle ctHandle
	contractionIndex  int // index into contraction elements
}

func (e *entry) String() string {
	return fmt.Sprintf("%X -> %X (ch:%x; ci:%d, ei:%d)",
		e.runes, e.elems, e.contractionHandle, e.contractionIndex, e.expansionIndex)
}

func (e *entry) skip() bool {
	return e.contraction()
}

func (e *entry) expansion() bool {
	return !e.decompose && len(e.elems) > 1
}

func (e *entry) contraction() bool {
	return len(e.runes) > 1
}

func (e *entry) contractionStarter() bool {
	return e.contractionHandle.n != 0
}

// A Builder builds collation tables.  It can generate both the root table and
// locale-specific tables defined as tailorings to the root table.
// The typical use case is to specify the data for the root table and all locale-specific
// tables using Add and AddTailoring before making any call to Build.  This allows
// Builder to ensure that a root table can support tailorings for each locale.
type Builder struct {
	entryMap map[string]*entry
	entry    []*entry
	t        *table
	err      error
}

// NewBuilder returns a new Builder.
func NewBuilder() *Builder {
	b := &Builder{
		entryMap: make(map[string]*entry),
	}
	return b
}

// Add adds an entry for the root collation element table, mapping 
// a slice of runes to a sequence of collation elements.
// A collation element is specified as list of weights: []int{primary, secondary, ...}.
// The entries are typically obtained from a collation element table
// as defined in http://www.unicode.org/reports/tr10/#Data_Table_Format.
// Note that the collation elements specified by colelems are only used
// as a guide.  The actual weights generated by Builder may differ.
func (b *Builder) Add(str []rune, colelems [][]int) error {
	e := &entry{
		runes: make([]rune, len(str)),
		elems: make([][]int, len(colelems)),
		str:   string(str),
	}
	copy(e.runes, str)
	for i, ce := range colelems {
		e.elems[i] = append(e.elems[i], ce...)
		if len(ce) == 0 {
			e.elems[i] = append(e.elems[i], []int{0, 0, 0, 0}...)
			break
		}
		if len(ce) == 1 {
			e.elems[i] = append(e.elems[i], defaultSecondary)
		}
		if len(ce) <= 2 {
			e.elems[i] = append(e.elems[i], defaultTertiary)
		}
		if len(ce) <= 3 {
			e.elems[i] = append(e.elems[i], ce[0])
		}
	}
	b.entryMap[string(str)] = e
	b.entry = append(b.entry, e)
	return nil
}

// AddTailoring defines a tailoring x <_level y for the given locale.
// For example, AddTailoring("se", "z", "ä", Primary) sorts "ä" after "z"
// at the primary level for Swedish.  AddTailoring("de", "ue", "ü", Secondary)
// sorts "ü" after "ue" at the secondary level for German.
// See http://www.unicode.org/reports/tr10/#Tailoring_Example for details
// on parametric tailoring.
func (b *Builder) AddTailoring(locale, x, y string, l collate.Level) error {
	// TODO: implement.
	return nil
}

func (b *Builder) baseColElem(e *entry) uint32 {
	ce := uint32(0)
	var err error
	switch {
	case e.expansion():
		ce, err = makeExpandIndex(e.expansionIndex)
	default:
		if e.decompose {
			log.Fatal("decompose should be handled elsewhere")
		}
		ce, err = makeCE(e.elems[0])
	}
	if err != nil {
		b.error(fmt.Errorf("%s: %X -> %X", err, e.runes, e.elems))
	}
	return ce
}

func (b *Builder) colElem(e *entry) uint32 {
	if e.skip() {
		log.Fatal("cannot build colElem for entry that should be skipped")
	}
	ce := uint32(0)
	var err error
	switch {
	case e.decompose:
		t1 := e.elems[0][2]
		t2 := 0
		if len(e.elems) > 1 {
			t2 = e.elems[1][2]
		}
		ce, err = makeDecompose(t1, t2)
	case e.contractionStarter():
		ce, err = makeContractIndex(e.contractionHandle, e.contractionIndex)
	default:
		if len(e.runes) > 1 {
			log.Fatal("colElem: contractions are handled in contraction trie")
		}
		ce = b.baseColElem(e)
	}
	if err != nil {
		b.error(err)
	}
	return ce
}

func (b *Builder) error(e error) {
	if e != nil {
		b.err = e
	}
}

func (b *Builder) build() (*table, error) {
	b.t = &table{}

	b.contractCJK()
	b.simplify()            // requires contractCJK
	b.processExpansions()   // requires simplify
	b.processContractions() // requires simplify
	b.buildTrie()           // requires process*

	if b.err != nil {
		return nil, b.err
	}
	return b.t, nil
}

// Build builds a Collator for the given locale.  To build the root table, set locale to "".
func (b *Builder) Build(locale string) (*collate.Collator, error) {
	t, err := b.build()
	if err != nil {
		return nil, err
	}
	// TODO: support multiple locales
	return collate.Init(t), nil
}

// Print prints all tables to a Go file that can be included in
// the Collate package.
func (b *Builder) Print(w io.Writer) (int, error) {
	t, err := b.build()
	if err != nil {
		return 0, err
	}
	// TODO: support multiple locales
	n, _, err := t.print(w, "root")
	return n, err
}

// reproducibleFromNFKD checks whether the given expansion could be generated
// from an NFKD expansion.
func reproducibleFromNFKD(e *entry, exp, nfkd [][]int) bool {
	// Length must be equal.
	if len(exp) != len(nfkd) {
		return false
	}
	for i, ce := range exp {
		// Primary and secondary values should be equal.
		if ce[0] != nfkd[i][0] || ce[1] != nfkd[i][1] {
			return false
		}
		// Tertiary values should be equal to maxTertiary for third element onwards.
		if i >= 2 && ce[2] != maxTertiary {
			return false
		}
	}
	return true
}

func equalCE(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < 3; i++ {
		if b[i] != a[i] {
			return false
		}
	}
	return true
}

func equalCEArrays(a, b [][]int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !equalCE(a[i], b[i]) {
			return false
		}
	}
	return true
}

// genColElems generates a collation element array from the runes in str. This
// assumes that all collation elements have already been added to the Builder.
func (b *Builder) genColElems(str string) [][]int {
	elems := [][]int{}
	for _, r := range []rune(str) {
		if ee, ok := b.entryMap[string(r)]; !ok {
			elem := []int{implicitPrimary(r), defaultSecondary, defaultTertiary, int(r)}
			elems = append(elems, elem)
		} else {
			elems = append(elems, ee.elems...)
		}
	}
	return elems
}

func (b *Builder) simplify() {
	// Runes that are a starter of a contraction should not be removed.
	// (To date, there is only Kannada character 0CCA.)
	keep := make(map[rune]bool)
	for _, e := range b.entry {
		if len(e.runes) > 1 {
			keep[e.runes[0]] = true
		}
	}
	// Remove entries for which the runes normalize (using NFD) to identical values.
	for _, e := range b.entry {
		s := e.str
		nfd := norm.NFD.String(s)
		if len(e.runes) > 1 || keep[e.runes[0]] || nfd == s {
			continue
		}
		if equalCEArrays(b.genColElems(nfd), e.elems) {
			delete(b.entryMap, s)
		}
	}
	// Remove entries in b.entry that were removed from b.entryMap
	k := 0
	for _, e := range b.entry {
		if _, ok := b.entryMap[e.str]; ok {
			b.entry[k] = e
			k++
		}
	}
	b.entry = b.entry[:k]
	// Tag entries for which the runes NFKD decompose to identical values.
	for _, e := range b.entry {
		s := e.str
		nfkd := norm.NFKD.String(s)
		if len(e.runes) > 1 || keep[e.runes[0]] || nfkd == s {
			continue
		}
		if reproducibleFromNFKD(e, e.elems, b.genColElems(nfkd)) {
			e.decompose = true
		}
	}
}

// convertLargeWeights converts collation elements with large 
// primaries (either double primaries or for illegal runes)
// to our own representation.
// See http://unicode.org/reports/tr10/#Implicit_Weights
func convertLargeWeights(elems [][]int) (res [][]int, err error) {
	const (
		firstLargePrimary = 0xFB40
		illegalPrimary    = 0xFFFE
		highBitsMask      = 0x3F
		lowBitsMask       = 0x7FFF
		lowBitsFlag       = 0x8000
		shiftBits         = 15
	)
	for i := 0; i < len(elems); i++ {
		ce := elems[i]
		p := ce[0]
		if p < firstLargePrimary {
			continue
		}
		if p >= illegalPrimary {
			ce[0] = illegalOffset + p - illegalPrimary
		} else {
			if i+1 >= len(elems) {
				return elems, fmt.Errorf("second part of double primary weight missing: %v", elems)
			}
			if elems[i+1][0]&lowBitsFlag == 0 {
				return elems, fmt.Errorf("malformed second part of double primary weight: %v", elems)
			}
			r := rune(((p & highBitsMask) << shiftBits) + elems[i+1][0]&lowBitsMask)
			ce[0] = implicitPrimary(r)
			for j := i + 1; j+1 < len(elems); j++ {
				elems[j] = elems[j+1]
			}
			elems = elems[:len(elems)-1]
		}
	}
	return elems, nil
}

// A CJK character C is represented in the DUCET as
//   [.FBxx.0020.0002.C][.BBBB.0000.0000.C]
// We will rewrite these characters to a single CE.
// We assume the CJK values start at 0x8000.
func (b *Builder) contractCJK() {
	for _, e := range b.entry {
		elms, err := convertLargeWeights(e.elems)
		e.elems = elms
		if err != nil {
			err = fmt.Errorf("%U: %s", e.runes, err)
		}
		b.error(err)
	}
}

// appendExpansion converts the given collation sequence to
// collation elements and adds them to the expansion table.
// It returns an index to the expansion table.
func (b *Builder) appendExpansion(e *entry) int {
	t := b.t
	i := len(t.expandElem)
	ce := uint32(len(e.elems))
	t.expandElem = append(t.expandElem, ce)
	for _, w := range e.elems {
		ce, err := makeCE(w)
		if err != nil {
			b.error(err)
			return -1
		}
		t.expandElem = append(t.expandElem, ce)
	}
	return i
}

// processExpansions extracts data necessary to generate
// the extraction tables.
func (b *Builder) processExpansions() {
	eidx := make(map[string]int)
	for _, e := range b.entry {
		if !e.expansion() {
			continue
		}
		key := fmt.Sprintf("%v", e.elems)
		i, ok := eidx[key]
		if !ok {
			i = b.appendExpansion(e)
			eidx[key] = i
		}
		e.expansionIndex = i
	}
}

func (b *Builder) processContractions() {
	// Collate contractions per starter rune.
	starters := []rune{}
	cm := make(map[rune][]*entry)
	for _, e := range b.entry {
		if e.contraction() {
			if len(e.str) > b.t.maxContractLen {
				b.t.maxContractLen = len(e.str)
			}
			r := e.runes[0]
			if _, ok := cm[r]; !ok {
				starters = append(starters, r)
			}
			cm[r] = append(cm[r], e)
		}
	}
	// Add entries of single runes that are at a start of a contraction.
	for _, e := range b.entry {
		if !e.contraction() {
			r := e.runes[0]
			if _, ok := cm[r]; ok {
				cm[r] = append(cm[r], e)
			}
		}
	}
	// Build the tries for the contractions.
	t := b.t
	handlemap := make(map[string]ctHandle)
	for _, r := range starters {
		l := cm[r]
		// Compute suffix strings. There are 31 different contraction suffix
		// sets for 715 contractions and 82 contraction starter runes as of
		// version 6.0.0.
		sufx := []string{}
		hasSingle := false
		for _, e := range l {
			if len(e.runes) > 1 {
				sufx = append(sufx, string(e.runes[1:]))
			} else {
				hasSingle = true
			}
		}
		if !hasSingle {
			b.error(fmt.Errorf("no single entry for starter rune %U found", r))
			continue
		}
		// Unique the suffix set.
		sort.Strings(sufx)
		key := strings.Join(sufx, "\n")
		handle, ok := handlemap[key]
		if !ok {
			var err error
			handle, err = t.contractTries.appendTrie(sufx)
			if err != nil {
				b.error(err)
			}
			handlemap[key] = handle
		}
		// Bucket sort entries in index order.
		es := make([]*entry, len(l))
		for _, e := range l {
			var o, sn int
			if len(e.runes) > 1 {
				str := []byte(string(e.runes[1:]))
				o, sn = t.contractTries.lookup(handle, str)
				if sn != len(str) {
					log.Fatalf("processContractions: unexpected length for '%X'; len=%d; want %d", []rune(string(str)), sn, len(str))
				}
			}
			if es[o] != nil {
				log.Fatalf("Multiple contractions for position %d for rune %U", o, e.runes[0])
			}
			es[o] = e
		}
		// Store info in entry for starter rune.
		es[0].contractionIndex = len(t.contractElem)
		es[0].contractionHandle = handle
		// Add collation elements for contractions.
		for _, e := range es {
			t.contractElem = append(t.contractElem, b.baseColElem(e))
		}
	}
}

func (b *Builder) buildTrie() {
	t := newNode()
	for _, e := range b.entry {
		if !e.skip() {
			ce := b.colElem(e)
			t.insert(e.runes[0], ce)
		}
	}
	i, err := t.generate()
	b.t.index = *i
	b.error(err)
}
