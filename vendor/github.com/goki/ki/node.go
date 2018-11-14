// Copyright (c) 2018, The GoKi Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Ki is the base element of GoKi Trees
// Ki = Tree in Japanese, and "Key" in English

package ki

import (
	"bytes"
	"encoding/gob"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"sync"
	"sync/atomic"
	"unsafe"

	"log"
	"reflect"
	"strings"

	"github.com/goki/ki/bitflag"
	"github.com/goki/ki/kit"
	"github.com/goki/prof"
	"github.com/jinzhu/copier"
)

// The Node implements the Ki interface and provides the core functionality
// for the GoKi tree -- use the Node as an embedded struct or as a struct
// field -- the embedded version supports full JSON save / load.
//
// The desc: key for fields is used by the GoGi GUI viewer for help / tooltip
// info -- add these to all your derived struct's fields.  See relevant docs
// for other such tags controlling a wide range of GUI and other functionality
// -- Ki makes extensive use of such tags.
type Node struct {
	Nm       string `copy:"-" label:"Name" desc:"Ki.Name() user-supplied name of this node -- can be empty or non-unique"`
	UniqueNm string `copy:"-" label:"UniqueName" desc:"Ki.UniqueName() automatically-updated version of Name that is guaranteed to be unique within the slice of Children within one Node -- used e.g., for saving Unique Paths in Ptr pointers"`
	Flag     int64  `copy:"-" json:"-" xml:"-" view:"-" desc:"bit flags for internal node state"`
	Props    Props  `xml:"-" copy:"-" label:"Properties" desc:"Ki.Properties() property map for arbitrary extensible properties, including style properties"`
	Par      Ki     `copy:"-" json:"-" xml:"-" label:"Parent" view:"-" desc:"Ki.Parent() parent of this node -- set automatically when this node is added as a child of parent"`
	Kids     Slice  `copy:"-" label:"Children" desc:"Ki.Children() list of children of this node -- all are set to have this node as their parent -- can reorder etc but generally use Ki Node methods to Add / Delete to ensure proper usage"`
	NodeSig  Signal `copy:"-" json:"-" xml:"-" desc:"Ki.NodeSignal() signal for node structure / state changes -- emits NodeSignals signals -- can also extend to custom signals (see signal.go) but in general better to create a new Signal instead"`
	Ths      Ki     `copy:"-" json:"-" xml:"-" view:"-" desc:"we need a pointer to ourselves as a Ki, which can always be used to extract the true underlying type of object when Node is embedded in other structs -- function receivers do not have this ability so this is necessary.  This is set to nil when deleted.  Typically use This() convenience accessor which protects against concurrent access."`
	index    int    `desc:"last value of our index -- used as a starting point for finding us in our parent next time -- is not guaranteed to be accurate!  use Index() method"`
}

// must register all new types so type names can be looked up by name -- also props
var KiT_Node = kit.Types.AddType(&Node{}, nil)

//////////////////////////////////////////////////////////////////////////
//  fmt.Stringer

// String implements the fmt.stringer interface -- returns the PathUnique of the node
func (n Node) String() string {
	return n.PathUnique()
}

//////////////////////////////////////////////////////////////////////////
//  Basic Ki fields

func (n *Node) This() Ki {
	if n == nil || n.IsDestroyed() {
		return nil
	}
	return n.Ths
}

func (n *Node) Init(this Ki) {
	kitype := KiType()
	n.ClearFlagMask(int64(UpdateFlagsMask))
	if n.Ths != this {
		n.Ths = this
		// we need to call this directly instead of FuncFields because we need the field name
		FlatFieldsValueFunc(this, func(stru interface{}, typ reflect.Type, field reflect.StructField, fieldVal reflect.Value) bool {
			if fieldVal.Kind() == reflect.Struct && kit.EmbeddedTypeImplements(field.Type, kitype) {
				fk := kit.PtrValue(fieldVal).Interface().(Ki)
				if fk != nil {
					fk.SetFlag(int(IsField))
					fk.InitName(fk, field.Name)
					fk.SetParent(this)
				}
			}
			return true
		})
	}
}

func (n *Node) InitName(k Ki, name string) {
	n.Init(k)
	n.SetName(name)
}

func (n *Node) ThisCheck() error {
	if n.This() == nil {
		err := fmt.Errorf("Ki Node %v ThisCheck: node has null 'this' pointer -- must call Init or InitName on root nodes!", n.PathUnique())
		log.Print(err)
		return err
	}
	return nil
}

func (n *Node) Type() reflect.Type {
	return reflect.TypeOf(n.This()).Elem()
}

func (n *Node) TypeEmbeds(t reflect.Type) bool {
	return kit.TypeEmbeds(n.Type(), t)
}

func (n *Node) Embed(t reflect.Type) Ki {
	if n == nil {
		return nil
	}
	es := kit.Embed(n.This(), t)
	if es != nil {
		k, ok := es.(Ki)
		if ok {
			return k
		}
		log.Printf("ki.Embed on: %v embedded struct is not a Ki type -- use kit.Embed for a more general version\n", n.PathUnique())
		return nil
	}
	return nil
}

func (n *Node) Name() string {
	return n.Nm
}

func (n *Node) UniqueName() string {
	return n.UniqueNm
}

// set name and unique name, ensuring unique name is unique..
func (n *Node) SetName(name string) bool {
	if n.Nm == name {
		return false
	}
	n.Nm = name
	n.SetUniqueName(name)
	if n.Par != nil {
		n.Par.UniquifyNames()
	}
	return true
}

func (n *Node) SetNameRaw(name string) {
	n.Nm = name
}

func (n *Node) SetUniqueName(name string) {
	n.UniqueNm = strings.Replace(strings.Replace(name, ".", "_", -1), "/", "_", -1)
}

// UniquifyPreserveNameLimit is the number of children below which a more
// expensive approach is taken to uniquify the names to guarantee unique
// paths, which preserves the original name wherever possible -- formatting of
// index assumes this limit is less than 1000
var UniquifyPreserveNameLimit = 100

// UniquifyNames makes sure that the names are unique -- the "deluxe" version
// preserves the regular User-given name but is relatively expensive (creates
// a map), so is only used below a certain size (UniquifyPreserveNameLimit =
// 100), above which the index is appended, guaranteeing uniqueness at the
// cost of making paths longer and less user-friendly
func (n *Node) UniquifyNames() {
	pr := prof.Start("ki.Node.UniquifyNames")
	defer pr.End()

	sz := len(n.Kids)
	if sz > UniquifyPreserveNameLimit {
		sfmt := "%v_%05d"
		switch {
		case sz > 9999999:
			sfmt = "%v_%10d"
		case sz > 999999:
			sfmt = "%v_%07d"
		case sz > 99999:
			sfmt = "%v_%06d"
		}
		for i, child := range n.Kids {
			child.SetUniqueName(fmt.Sprintf(sfmt, child.Name(), i))
		}
		return
	}
	nmap := make(map[string]int, sz)
	for i, child := range n.Kids {
		if len(child.UniqueName()) == 0 {
			if n.Par != nil {
				child.SetUniqueName(fmt.Sprintf("%v_%03d", n.Par.UniqueName(), i))
			} else {
				child.SetUniqueName(fmt.Sprintf("c%03d", i))
			}
		}
		if _, taken := nmap[child.UniqueName()]; taken {
			child.SetUniqueName(fmt.Sprintf("%v_%03d", child.UniqueName(), i))
		} else {
			nmap[child.UniqueName()] = i
		}
	}
}

//////////////////////////////////////////////////////////////////////////
//  Parents

func (n *Node) Parent() Ki {
	return n.Par
}

func (n *Node) SetParent(parent Ki) {
	n.Par = parent
	if parent != nil && !parent.OnlySelfUpdate() {
		parup := parent.IsUpdating()
		n.FuncDownMeFirst(0, nil, func(k Ki, level int, d interface{}) bool {
			k.SetFlagState(parup, int(Updating))
			return true
		})
	}
}

func (n *Node) IsRoot() bool {
	if n.This() == nil || n.Par == nil || n.Par.This() == nil {
		return true
	}
	return false
}

func (n *Node) Root() Ki {
	if n.IsRoot() {
		return n.This()
	}
	return n.Par.Root()
}

func (n *Node) FieldRoot() Ki {
	var root Ki
	gotField := false
	n.FuncUpParent(0, n, func(k Ki, level int, d interface{}) bool {
		if !gotField {
			if k.IsField() {
				gotField = true
			}
			return true
		} else {
			if !k.IsField() {
				root = k
				return false
			}
		}
		return true
	})
	return root
}

func (n *Node) IndexInParent() (int, bool) {
	if n.Par == nil {
		return -1, false
	}
	var ok bool
	n.index, ok = n.Par.Children().IndexOf(n.This(), n.index) // very fast if index is close..
	return n.index, ok
}

func (n *Node) ParentLevel(par Ki) int {
	parLev := -1
	n.FuncUpParent(0, n, func(k Ki, level int, d interface{}) bool {
		if k == par {
			parLev = level
			return false
		}
		return true
	})
	return parLev
}

func (n *Node) HasParent(par Ki) bool {
	return n.ParentLevel(par) != -1
}

func (n *Node) ParentByName(name string) (Ki, bool) {
	if n.IsRoot() {
		return nil, false
	}
	if n.Par.Name() == name {
		return n.Par, true
	}
	return n.Par.ParentByName(name)
}

func (n *Node) ParentByType(t reflect.Type, embeds bool) (Ki, bool) {
	if n.IsRoot() {
		return nil, false
	}
	if embeds {
		if n.Par.TypeEmbeds(t) {
			return n.Par, true
		}
	} else {
		if n.Par.Type() == t {
			return n.Par, true
		}
	}
	return n.Par.ParentByType(t, embeds)
}

func (n *Node) KiFieldByName(name string) (Ki, bool) {
	v := reflect.ValueOf(n.This()).Elem()
	f := v.FieldByName(name)
	if !f.IsValid() {
		return nil, false
	}
	if !kit.EmbeddedTypeImplements(f.Type(), KiType()) {
		return nil, false
	}
	return kit.PtrValue(f).Interface().(Ki), true
}

//////////////////////////////////////////////////////////////////////////
//  Children

func (n *Node) HasChildren() bool {
	return len(n.Kids) > 0
}

func (n *Node) Children() *Slice {
	return &n.Kids
}

func (n *Node) IsValidIndex(idx int) bool {
	return n.Kids.IsValidIndex(idx)
}

func (n *Node) Child(idx int) (Ki, bool) {
	return n.Kids.Elem(idx)
}

func (n *Node) KnownChild(idx int) Ki {
	return n.Kids[idx]
}

func (n *Node) ChildByName(name string, startIdx int) (Ki, bool) {
	return n.Kids.ElemByName(name, startIdx)
}

func (n *Node) KnownChildByName(name string, startIdx int) Ki {
	return n.Kids.KnownElemByName(name, startIdx)
}

//////////////////////////////////////////////////////////////////////////
//  Paths

func (n *Node) Path() string {
	if n.Par != nil {
		if n.IsField() {
			return n.Par.Path() + "." + n.Nm
		} else {
			return n.Par.Path() + "/" + n.Nm
		}
	}
	return "/" + n.Nm
}

func (n *Node) PathUnique() string {
	if n.Par != nil {
		if n.IsField() {
			return n.Par.PathUnique() + "." + n.UniqueNm
		} else {
			return n.Par.PathUnique() + "/" + n.UniqueNm
		}
	}
	return "/" + n.UniqueNm
}

func (n *Node) PathFrom(par Ki) string {
	if n.Par != nil && n.Par != par {
		if n.IsField() {
			return n.Par.PathFrom(par) + "." + n.Nm
		} else {
			return n.Par.PathFrom(par) + "/" + n.Nm
		}
	}
	return "/" + n.Nm
}

func (n *Node) PathFromUnique(par Ki) string {
	if n.Par != nil && n.Par != par {
		if n.IsField() {
			return n.Par.PathFromUnique(par) + "." + n.UniqueNm
		} else {
			return n.Par.PathFromUnique(par) + "/" + n.UniqueNm
		}
	}
	return "/" + n.UniqueNm
}

func (n *Node) FindPathUnique(path string) (Ki, bool) {
	if n.Par != nil { // we are not root..
		myp := n.PathUnique()
		path = strings.TrimPrefix(path, myp)
	}
	curn := Ki(n)
	pels := strings.Split(strings.Trim(strings.TrimSpace(path), "\""), "/")
	for i, pe := range pels {
		if len(pe) == 0 {
			continue
		}
		if i <= 1 && curn.UniqueName() == pe {
			continue
		}
		if strings.Contains(pe, ".") { // has fields
			fels := strings.Split(pe, ".")
			// find the child first, then the fields
			idx, ok := curn.Children().IndexByUniqueName(fels[0], 0)
			if !ok {
				return nil, false
			}
			curn = (*(curn.Children()))[idx]
			for i := 1; i < len(fels); i++ {
				fe := fels[i]
				fk, ok := curn.KiFieldByName(fe)
				if !ok {
					return nil, false
				}
				curn = fk
			}
		} else {
			idx, ok := curn.Children().IndexByUniqueName(pe, 0)
			if !ok {
				return nil, false
			}
			curn = (*(curn.Children()))[idx]
		}
	}
	return curn, true
}

//////////////////////////////////////////////////////////////////////////
//  Adding, Inserting Children

func (n *Node) SetChildType(t reflect.Type) error {
	if !reflect.PtrTo(t).Implements(reflect.TypeOf((*Ki)(nil)).Elem()) {
		err := fmt.Errorf("Ki Node %v SetChildType: type does not implement the Ki interface -- must -- type passed is: %v", n.PathUnique(), t.Name())
		log.Print(err)
		return err
	}
	n.SetProp("ChildType", t)
	return nil
}

// check if it is safe to add child -- it cannot be a parent of us -- prevent loops!
func (n *Node) AddChildCheck(kid Ki) error {
	var err error
	n.FuncUp(0, n, func(k Ki, level int, d interface{}) bool {
		if k == kid {
			err = fmt.Errorf("Ki Node Attempt to add child to node %v that is my own parent -- no cycles permitted!\n", (d.(Ki)).PathUnique())
			log.Printf("%v", err)
			return false
		}
		return true
	})
	return err
}

// after adding child -- signals etc
func (n *Node) addChildImplPost(kid Ki) {
	oldPar := kid.Parent()
	kid.SetParent(n.This()) // key to set new parent before deleting: indicates move instead of delete
	if oldPar != nil {
		oldPar.DeleteChild(kid, false)
		kid.SetFlag(int(ChildMoved))
	} else {
		kid.SetFlag(int(ChildAdded))
	}
}

func (n *Node) AddChildImpl(kid Ki) error {
	if err := n.ThisCheck(); err != nil {
		return err
	}
	if err := n.AddChildCheck(kid); err != nil {
		return err
	}
	kid.Init(kid)
	n.Kids = append(n.Kids, kid)
	n.addChildImplPost(kid)
	return nil
}

func (n *Node) InsertChildImpl(kid Ki, at int) error {
	if err := n.ThisCheck(); err != nil {
		return err
	}
	if err := n.AddChildCheck(kid); err != nil {
		return err
	}
	kid.Init(kid)
	n.Kids.Insert(kid, at)
	n.addChildImplPost(kid)
	return nil
}

func (n *Node) AddChild(kid Ki) error {
	updt := n.UpdateStart()
	err := n.AddChildImpl(kid)
	if err == nil {
		n.SetFlag(int(ChildAdded))
		if kid.UniqueName() == "" {
			kid.SetUniqueName(kid.Name())
		}
		n.UniquifyNames()
	}
	n.UpdateEnd(updt)
	return err
}

func (n *Node) InsertChild(kid Ki, at int) error {
	updt := n.UpdateStart()
	err := n.InsertChildImpl(kid, at)
	if err == nil {
		n.SetFlag(int(ChildAdded))
		if kid.UniqueName() == "" {
			kid.SetUniqueName(kid.Name())
		}
		n.UniquifyNames()
	}
	n.UpdateEnd(updt)
	return err
}

func (n *Node) NewOfType(typ reflect.Type) Ki {
	if err := n.ThisCheck(); err != nil {
		return nil
	}
	if typ == nil {
		ct, ok := n.PropInherit("ChildType", false, true) // no inherit but yes from type
		if ok {
			if ctt, ok := ct.(reflect.Type); ok {
				typ = ctt
			}
		}
	}
	if typ == nil {
		typ = n.Type() // make us by default
	}
	nkid := reflect.New(typ).Interface()
	kid, _ := nkid.(Ki)
	return kid
}

func (n *Node) AddNewChild(typ reflect.Type, name string) Ki {
	updt := n.UpdateStart()
	kid := n.NewOfType(typ)
	err := n.AddChildImpl(kid)
	if err == nil {
		kid.SetName(name)
		n.SetFlag(int(ChildAdded))
	}
	n.UpdateEnd(updt)
	return kid
}

func (n *Node) InsertNewChild(typ reflect.Type, at int, name string) Ki {
	updt := n.UpdateStart()
	kid := n.NewOfType(typ)
	err := n.InsertChildImpl(kid, at)
	if err == nil {
		kid.SetName(name)
		n.SetFlag(int(ChildAdded))
	}
	n.UpdateEnd(updt)
	return kid
}

func (n *Node) InsertNewChildUnique(typ reflect.Type, at int, name string) Ki {
	updt := n.UpdateStart()
	kid := n.NewOfType(typ)
	err := n.InsertChildImpl(kid, at)
	if err == nil {
		kid.SetNameRaw(name)
		kid.SetUniqueName(name)
		n.SetFlag(int(ChildAdded))
	}
	n.UpdateEnd(updt)
	return kid
}

func (n *Node) SetChild(kid Ki, idx int, name string) error {
	if !n.Kids.IsValidIndex(idx) {
		return fmt.Errorf("ki.SetChild for node: %v index invalid: %v -- size: %v\n", n.PathUnique(), idx, len(n.Kids))
	}
	if name != "" {
		kid.InitName(kid, name)
	} else {
		kid.Init(kid)
	}
	n.Kids[idx] = kid
	kid.SetParent(n.This())
	return nil
}

func (n *Node) MoveChild(from, to int) bool {
	updt := n.UpdateStart()
	ok := n.Kids.Move(from, to)
	if ok {
		n.SetFlag(int(ChildMoved))
	}
	n.UpdateEnd(updt)
	return ok
}

func (n *Node) SetNChildren(trgn int, typ reflect.Type, nameStub string) (mods, updt bool) {
	mods, updt = false, false
	sz := len(n.Kids)
	if trgn == sz {
		return
	}
	for sz > trgn {
		if !mods {
			mods = true
			updt = n.UpdateStart()
		}
		sz--
		n.DeleteChildAtIndex(sz, true)
	}
	for sz < trgn {
		if !mods {
			mods = true
			updt = n.UpdateStart()
		}
		nm := fmt.Sprintf("%v%v", nameStub, sz)
		n.InsertNewChildUnique(typ, sz, nm)
		sz++
	}
	return
}

func (n *Node) ConfigChildren(config kit.TypeAndNameList, uniqNm bool) (mods, updt bool) {
	return n.Kids.Config(n.This(), config, uniqNm)
}

//////////////////////////////////////////////////////////////////////////
//  Deleting Children

func (n *Node) DeleteChildAtIndex(idx int, destroy bool) bool {
	child, ok := n.Child(idx)
	if !ok {
		return false
	}
	updt := n.UpdateStart()
	n.SetFlag(int(ChildDeleted))
	if child.Parent() == n.This() {
		// only deleting if we are still parent -- change parent first to
		// signal move delete is always sent live to affected node without
		// update blocking note: children of child etc will not send a signal
		// at this point -- only later at destroy -- up to this parent to
		// manage all that
		child.SetFlag(int(NodeDeleted))
		child.NodeSignal().Emit(child, int64(NodeSignalDeleting), nil)
		child.SetParent(nil)
	}
	n.Kids.DeleteAtIndex(idx)
	if destroy {
		DelMgr.Add(child)
	}
	child.UpdateReset() // it won't get the UpdateEnd from us anymore -- init fresh in any case
	n.UpdateEnd(updt)
	return true
}

func (n *Node) DeleteChild(child Ki, destroy bool) bool {
	idx, ok := n.Kids.IndexOf(child, 0)
	if !ok {
		return false
	}
	return n.DeleteChildAtIndex(idx, destroy)
}

func (n *Node) DeleteChildByName(name string, destroy bool) (Ki, bool) {
	idx, ok := n.Kids.IndexByName(name, 0)
	if !ok {
		return nil, false
	}
	child := n.Kids[idx]
	n.DeleteChildAtIndex(idx, destroy)
	return child, true
}

func (n *Node) DeleteChildren(destroy bool) {
	updt := n.UpdateStart()
	n.SetFlag(int(ChildrenDeleted))
	for _, child := range n.Kids {
		child.SetFlag(int(NodeDeleted))
		child.NodeSignal().Emit(child, int64(NodeSignalDeleting), nil)
		child.SetParent(nil)
		child.UpdateReset()
	}
	if destroy {
		DelMgr.Add(n.Kids...)
	}
	n.Kids = n.Kids[:0] // preserves capacity of list
	n.UpdateEnd(updt)
}

func (n *Node) Delete(destroy bool) {
	if n.Par == nil {
		if destroy {
			n.Destroy()
		}
	} else {
		n.Par.DeleteChild(n.This(), destroy)
	}
}

func (n *Node) Destroy() {
	// fmt.Printf("Destroying: %v %T %p Kids: %v\n", n.PathUnique(), n.This(), n.This(), len(n.Kids))
	if n.This() == nil { // already dead!
		return
	}
	n.NodeSig.Emit(n.This(), int64(NodeSignalDestroying), nil)
	n.DisconnectAll()
	n.DeleteChildren(true) // first delete all my children
	// and destroy all my fields
	n.FuncFields(0, nil, func(k Ki, level int, d interface{}) bool {
		k.Destroy()
		return true
	})
	DelMgr.DestroyDeleted() // then destroy all those kids
	// extra step to delete all the slices and maps -- super friendly to GC :)
	// note: not safe at this point!
	// FlatFieldsValueFunc(n.This(), func(stru interface{}, typ reflect.Type, field reflect.StructField, fieldVal reflect.Value) bool {
	// 	if fieldVal.Kind() == reflect.Slice || fieldVal.Kind() == reflect.Map {
	// 		fieldVal.Set(reflect.Zero(fieldVal.Type())) // set to nil
	// 	}
	// 	return true
	// })
	n.SetFlag(int(NodeDestroyed))
	n.Ths = nil // last gasp: lose our own sense of self..
	// note: above is thread-safe because This() accessor checks Destroyed
}

//////////////////////////////////////////////////////////////////////////
//  Flags

func (n *Node) Flags() int64 {
	return atomic.LoadInt64(&n.Flag)
}

func (n *Node) HasFlag(flag int) bool {
	return bitflag.HasAtomic(&n.Flag, flag)
}

func (n *Node) HasAnyFlag(flag ...int) bool {
	return bitflag.HasAnyAtomic(&n.Flag, flag...)
}

func (n *Node) HasAllFlags(flag ...int) bool {
	return bitflag.HasAllAtomic(&n.Flag, flag...)
}

func (n *Node) SetFlag(flag ...int) {
	bitflag.SetAtomic(&n.Flag, flag...)
}

func (n *Node) SetFlagState(on bool, flag ...int) {
	bitflag.SetStateAtomic(&n.Flag, on, flag...)
}

func (n *Node) SetFlagMask(mask int64) {
	bitflag.SetMaskAtomic(&n.Flag, mask)
}

func (n *Node) ClearFlag(flag ...int) {
	bitflag.ClearAtomic(&n.Flag, flag...)
}

func (n *Node) ClearFlagMask(mask int64) {
	bitflag.ClearMaskAtomic(&n.Flag, mask)
}

func (n *Node) IsUpdating() bool {
	return bitflag.HasAtomic(&n.Flag, int(Updating))
}

func (n *Node) IsField() bool {
	return bitflag.HasAtomic(&n.Flag, int(IsField))
}

func (n *Node) OnlySelfUpdate() bool {
	return bitflag.HasAtomic(&n.Flag, int(OnlySelfUpdate))
}

func (n *Node) SetOnlySelfUpdate() {
	n.SetFlag(int(OnlySelfUpdate))
}

func (n *Node) IsDeleted() bool {
	return bitflag.HasAtomic(&n.Flag, int(NodeDeleted))
}

func (n *Node) IsDestroyed() bool {
	return bitflag.HasAtomic(&n.Flag, int(NodeDestroyed))
}

//////////////////////////////////////////////////////////////////////////
//  Property interface with inheritance -- nodes can inherit props from parents

func (n *Node) Properties() *Props {
	return &n.Props
}

func (n *Node) SetProp(key string, val interface{}) {
	if n.Props == nil {
		n.Props = make(Props)
	}
	n.Props[key] = val
}

func (n *Node) SetProps(props Props, update bool) {
	if n.Props == nil {
		n.Props = make(Props)
	}
	for key, val := range props {
		n.Props[key] = val
	}
	if update {
		n.SetFlag(int(PropUpdated))
		n.UpdateSig()
	}
}

func (n *Node) SetPropUpdate(key string, val interface{}) {
	n.SetFlag(int(PropUpdated))
	n.SetProp(key, val)
	n.UpdateSig()
}

func (n *Node) SetPropChildren(key string, val interface{}) {
	for _, k := range n.Kids {
		k.SetProp(key, val)
	}
}

func (n *Node) Prop(key string) (interface{}, bool) {
	v, ok := n.Props[key]
	return v, ok
}

func (n *Node) KnownProp(key string) interface{} {
	return n.Props[key]
}

func (n *Node) PropInherit(key string, inherit, typ bool) (interface{}, bool) {
	v, ok := n.Props[key]
	if ok {
		return v, ok
	}
	if inherit && n.Par != nil {
		v, ok = n.Par.PropInherit(key, inherit, typ)
		if ok {
			return v, ok
		}
	}
	if typ {
		return kit.Types.Prop(n.Type(), key)
	}
	return nil, false
}

func (n *Node) DeleteProp(key string) {
	if n.Props == nil {
		return
	}
	delete(n.Props, key)
}

func (n *Node) DeleteAllProps(cap int) {
	if n.Props != nil {
		if cap == 0 {
			n.Props = nil
		} else {
			n.Props = make(Props, cap)
		}
	}
}

func init() {
	gob.Register(Props{})
}

func (n *Node) CopyPropsFrom(from Ki, deep bool) error {
	if *(from.Properties()) == nil {
		return nil
	}
	if n.Props == nil {
		n.Props = make(Props)
	}
	fmP := *(from.Properties())
	if deep {
		// code from https://gist.github.com/soroushjp/0ec92102641ddfc3ad5515ca76405f4d
		var buf bytes.Buffer
		enc := gob.NewEncoder(&buf)
		dec := gob.NewDecoder(&buf)
		err := enc.Encode(fmP)
		if err != nil {
			return err
		}
		err = dec.Decode(&n.Props)
		if err != nil {
			return err
		}
		return nil
	} else {
		for k, v := range fmP {
			n.Props[k] = v
		}
	}
	return nil
}

func (n *Node) PropTag() string {
	return ""
}

//////////////////////////////////////////////////////////////////////////
//  Tree walking and state updating

func (n *Node) Fields() []uintptr {
	// we store the offsets for the fields in type properties
	tprops := *kit.Types.Properties(n.Type(), true) // true = makeNew
	pnm := "__FieldOffs"
	if foff, ok := kit.TypeProp(tprops, pnm); ok {
		return foff.([]uintptr)
	}
	foff := make([]uintptr, 0)
	kitype := KiType()
	FlatFieldsValueFunc(n.This(), func(stru interface{}, typ reflect.Type, field reflect.StructField, fieldVal reflect.Value) bool {
		if fieldVal.Kind() == reflect.Struct && kit.EmbeddedTypeImplements(field.Type, kitype) {
			foff = append(foff, field.Offset)
		}
		return true
	})
	kit.SetTypeProp(tprops, pnm, foff)
	return foff
}

// Node version of this function from kit/embeds.go
func FlatFieldsValueFunc(stru interface{}, fun func(stru interface{}, typ reflect.Type, field reflect.StructField, fieldVal reflect.Value) bool) bool {
	v := kit.NonPtrValue(reflect.ValueOf(stru))
	typ := v.Type()
	if typ == nil || typ == KiT_Node { // this is only diff from embeds.go version -- prevent processing of any Node fields
		return true
	}
	rval := true
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		vf := v.Field(i)
		if !vf.CanInterface() {
			continue
		}
		vfi := vf.Interface() // todo: check for interfaceablity etc
		if vfi == nil || vfi == stru {
			continue
		}
		if f.Type.Kind() == reflect.Struct && f.Anonymous && kit.PtrType(f.Type) != KiT_Node {
			rval = FlatFieldsValueFunc(kit.PtrValue(vf).Interface(), fun)
			if !rval {
				break
			}
		} else {
			rval = fun(vfi, typ, f, vf)
			if !rval {
				break
			}
		}
	}
	return rval
}

func (n *Node) FuncFields(level int, data interface{}, fun Func) {
	if n.This() == nil {
		return
	}
	op := reflect.ValueOf(n.This()).Pointer()
	foffs := n.Fields()
	for _, fo := range foffs {
		fn := (*Node)(unsafe.Pointer(op + fo))
		fun(fn.This(), level, data)
	}
}

func (n *Node) GoFuncFields(level int, data interface{}, fun Func) {
	if n.This() == nil {
		return
	}
	op := reflect.ValueOf(n.This()).Pointer()
	foffs := n.Fields()
	for _, fo := range foffs {
		fn := (*Node)(unsafe.Pointer(op + fo))
		go fun(fn.This(), level, data)
	}
}

func (n *Node) FuncUp(level int, data interface{}, fun Func) bool {
	if !fun(n.This(), level, data) { // false return means stop
		return false
	}
	level++
	if n.Parent() != nil && n.Parent() != n.This() { // prevent loops
		return n.Parent().FuncUp(level, data, fun)
	}
	return true
}

func (n *Node) FuncUpParent(level int, data interface{}, fun Func) bool {
	if n.IsRoot() {
		return true
	}
	if !fun(n.Parent(), level, data) { // false return means stop
		return false
	}
	level++
	return n.Parent().FuncUpParent(level, data, fun)
}

func (n *Node) FuncDownMeFirst(level int, data interface{}, fun Func) bool {
	if n.This() == nil {
		return false
	}
	if !fun(n.This(), level, data) { // false return means stop
		return false
	}
	level++
	n.FuncFields(level, data, func(k Ki, level int, d interface{}) bool {
		k.FuncDownMeFirst(level, data, fun)
		return true
	})
	for _, child := range *n.Children() {
		child.FuncDownMeFirst(level, data, fun) // don't care about their return values
	}
	return true
}

func (n *Node) FuncDownDepthFirst(level int, data interface{}, doChildTestFunc Func, fun Func) {
	if n.This() == nil {
		return
	}
	level++
	for _, child := range *n.Children() {
		if child.This() != nil {
			if doChildTestFunc(child.This(), level, data) { // test if we should run on this child
				child.FuncDownDepthFirst(level, data, doChildTestFunc, fun)
			}
		}
	}
	n.FuncFields(level, data, func(k Ki, level int, d interface{}) bool {
		if k.This() != nil {
			if doChildTestFunc(k, level, data) { // test if we should run on this child
				k.FuncDownDepthFirst(level, data, doChildTestFunc, fun)
			}
			fun(k, level, data)
		}
		return true
	})
	level--
	fun(n.This(), level, data) // can't use the return value at this point
}

func (n *Node) FuncDownBreadthFirst(level int, data interface{}, fun Func) {
	if n.This() == nil {
		return
	}
	dontMap := make(map[int]struct{}) // map of who NOT to process further -- default is false for map so reverse
	level++
	for i, child := range *n.Children() {
		if child.This() == nil || !fun(child, level, data) {
			// false return means stop
			dontMap[i] = struct{}{}
		} else {
			child.FuncFields(level+1, data, func(k Ki, level int, d interface{}) bool {
				k.FuncDownBreadthFirst(level+1, data, fun)
				fun(k, level+1, data)
				return true
			})
		}
	}
	for i, child := range *n.Children() {
		if _, has := dontMap[i]; has {
			continue
		}
		child.FuncDownBreadthFirst(level, data, fun)
	}
}

func (n *Node) GoFuncDown(level int, data interface{}, fun Func) {
	if n.This() == nil {
		return
	}
	go fun(n.This(), level, data)
	level++
	n.GoFuncFields(level, data, fun)
	for _, child := range *n.Children() {
		child.GoFuncDown(level, data, fun)
	}
}

// func (n *Node) GoFuncDownWait(level int, data interface{}, fun Func) {
// if n.This() == nil {
// 	return
// }
// 	// todo: use channel or something to wait
// 	go fun(n.This(), level, data)
// 	level++
// 	n.GoFuncFields(level, data, fun)
// 	for _, child := range *n.Children() {
// 		child.GoFuncDown(level, data, fun)
// 	}
// }

//////////////////////////////////////////////////////////////////////////
//  State update signaling -- automatically consolidates all changes across
//   levels so there is only one update at highest level of modification
//   All modification starts with UpdateStart() and ends with UpdateEnd()

// after an UpdateEnd, DestroyDeleted is called

func (n *Node) NodeSignal() *Signal {
	return &n.NodeSig
}

func (n *Node) UpdateStart() bool {
	if n.IsUpdating() || n.IsDestroyed() {
		return false
	}
	if n.OnlySelfUpdate() {
		n.SetFlag(int(Updating))
	} else {
		n.FuncDownMeFirst(0, nil, func(k Ki, level int, d interface{}) bool {
			if !k.IsUpdating() {
				k.ClearFlagMask(int64(UpdateFlagsMask))
				k.SetFlag(int(Updating))
				return true // keep going down
			} else {
				return false // bail -- already updating
			}
		})
	}
	return true
}

func (n *Node) UpdateEnd(updt bool) {
	if !updt {
		return
	}
	if n.IsDestroyed() || n.IsDeleted() {
		return
	}
	if n.HasAnyFlag(int(ChildDeleted), int(ChildrenDeleted)) {
		DelMgr.DestroyDeleted()
	}
	if n.OnlySelfUpdate() {
		n.ClearFlag(int(Updating))
		n.NodeSignal().Emit(n.This(), int64(NodeSignalUpdated), n.Flags())
	} else {
		n.FuncDownMeFirst(0, nil, func(k Ki, level int, d interface{}) bool {
			k.ClearFlag(int(Updating)) // todo: could check first and break here but good to ensure all clear
			return true
		})
		n.NodeSignal().Emit(n.This(), int64(NodeSignalUpdated), n.Flags())
	}
}

func (n *Node) UpdateEndNoSig(updt bool) {
	if !updt {
		return
	}
	if n.IsDestroyed() || n.IsDeleted() {
		return
	}
	if n.HasAnyFlag(int(ChildDeleted), int(ChildrenDeleted)) {
		DelMgr.DestroyDeleted()
	}
	if n.OnlySelfUpdate() {
		n.ClearFlag(int(Updating))
		// n.NodeSignal().Emit(n.This(), int64(NodeSignalUpdated), n.Flags())
	} else {
		n.FuncDownMeFirst(0, nil, func(k Ki, level int, d interface{}) bool {
			k.ClearFlag(int(Updating)) // todo: could check first and break here but good to ensure all clear
			return true
		})
		// n.NodeSignal().Emit(n.This(), int64(NodeSignalUpdated), n.Flags())
	}
}

func (n *Node) UpdateSig() bool {
	if n.IsUpdating() || n.IsDestroyed() {
		return false
	}
	n.NodeSignal().Emit(n.This(), int64(NodeSignalUpdated), n.Flags())
	return true
}

func (n *Node) UpdateReset() {
	if n.OnlySelfUpdate() {
		n.ClearFlag(int(Updating))
	} else {
		n.FuncDownMeFirst(0, nil, func(k Ki, level int, d interface{}) bool {
			k.ClearFlag(int(Updating))
			return true
		})
	}
}

func (n *Node) Disconnect() {
	n.NodeSig.DisconnectAll()
	FlatFieldsValueFunc(n.This(), func(stru interface{}, typ reflect.Type, field reflect.StructField, fieldVal reflect.Value) bool {
		switch {
		case fieldVal.Kind() == reflect.Interface:
			if field.Name != "This" { // reserve that for last step in Destroy
				fieldVal.Set(reflect.Zero(fieldVal.Type())) // set to nil
			}
		case fieldVal.Kind() == reflect.Ptr:
			fieldVal.Set(reflect.Zero(fieldVal.Type())) // set to nil
		case fieldVal.Type() == KiT_Signal:
			if fs, ok := kit.PtrValue(fieldVal).Interface().(*Signal); ok {
				// fmt.Printf("ki.Node: %v Type: %T Disconnecting signal field: %v\n", n.Name(), n.This(), field.Name)
				fs.DisconnectAll()
			}
		case fieldVal.Type() == KiT_Ptr:
			if pt, ok := kit.PtrValue(fieldVal).Interface().(*Ptr); ok {
				pt.Reset()
			}
		}
		return true
	})
}

func (n *Node) DisconnectAll() {
	n.FuncDownMeFirst(0, nil, func(k Ki, level int, d interface{}) bool {
		k.Disconnect()
		return true
	})
}

//////////////////////////////////////////////////////////////////////////
//  Field Value setting with notification

func (n *Node) SetField(field string, val interface{}) bool {
	fv := kit.FlatFieldValueByName(n.This(), field)
	if !fv.IsValid() {
		log.Printf("ki.SetField, could not find field %v on node %v\n", field, n.PathUnique())
		return false
	}
	updt := n.UpdateStart()
	ok := kit.SetRobust(kit.PtrValue(fv).Interface(), val)
	if ok {
		n.SetFlag(int(FieldUpdated))
	}
	n.UpdateEnd(updt)
	return ok
}

func (n *Node) SetFieldDown(field string, val interface{}) {
	updt := n.UpdateStart()
	n.FuncDownMeFirst(0, nil, func(k Ki, level int, d interface{}) bool {
		k.SetField(field, val)
		return true
	})
	n.UpdateEnd(updt)
}

func (n *Node) SetFieldUp(field string, val interface{}) {
	updt := n.UpdateStart()
	n.FuncUp(0, nil, func(k Ki, level int, d interface{}) bool {
		k.SetField(field, val)
		return true
	})
	n.UpdateEnd(updt)
}

func (n *Node) FieldByName(field string) interface{} {
	return kit.FlatFieldInterfaceByName(n.This(), field)
}

func (n *Node) FieldTag(field, tag string) string {
	return kit.FlatFieldTag(n.Type(), field, tag)
}

//////////////////////////////////////////////////////////////////////////
//  Deep Copy / Clone

// note: we use the copy from direction as the receiver is modifed whereas the
// from is not and assignment is typically in same direction

func (n *Node) CopyFrom(from Ki) error {
	if from == nil {
		err := fmt.Errorf("Ki Node CopyFrom into %v -- null 'from' source\n", n.PathUnique())
		log.Print(err)
		return err
	}
	mypath := n.PathUnique()
	fmpath := from.PathUnique()
	if n.Type() != from.Type() {
		err := fmt.Errorf("Ki Node Copy to %v from %v -- must have same types, but %v != %v\n", mypath, fmpath, n.Type().Name(), from.Type().Name())
		log.Print(err)
		return err
	}
	updt := n.UpdateStart()
	n.SetFlag(int(NodeCopied))
	sameTree := (n.Root() == from.Root())
	from.GetPtrPaths()
	err := n.CopyFromRaw(from)
	// DelMgr.DestroyDeleted() // in case we deleted some kiddos
	if err != nil {
		n.UpdateEnd(updt)
		return err
	}
	if sameTree {
		n.UpdatePtrPaths(fmpath, mypath, true)
	}
	n.SetPtrsFmPaths()
	n.UpdateEnd(updt)
	return nil
}

func (n *Node) Clone() Ki {
	nki := n.NewOfType(n.Type())
	nki.InitName(nki, n.Nm)
	nki.CopyFrom(n.This())
	return nki
}

// use ConfigChildren to recreate source children
func (n *Node) CopyMakeChildrenFrom(from Ki) {
	sz := len(*from.Children())
	if sz > 0 {
		cfg := make(kit.TypeAndNameList, sz)
		for i, kid := range *from.Children() {
			cfg[i].Type = kid.Type()
			cfg[i].Name = kid.UniqueName() // use unique so guaranteed to have something
		}
		mods, updt := n.ConfigChildren(cfg, true) // use unique names -- this means name = uniquname
		for i, kid := range *from.Children() {
			mkid := n.Kids[i]
			mkid.SetNameRaw(kid.Name()) // restore orig user-names
		}
		if mods {
			n.UpdateEnd(updt)
		}
	} else {
		n.DeleteChildren(true)
	}
}

// copy from primary fields of from to to, recursively following anonymous embedded structs
func (n *Node) CopyFieldsFrom(to interface{}, from interface{}) {
	kitype := KiType()
	tv := kit.NonPtrValue(reflect.ValueOf(to))
	sv := kit.NonPtrValue(reflect.ValueOf(from))
	typ := tv.Type()
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		tf := tv.Field(i)
		if !tf.CanInterface() {
			continue
		}
		ctag := f.Tag.Get("copy")
		if ctag == "-" {
			continue
		}
		sf := sv.Field(i)
		tfpi := kit.PtrValue(tf).Interface()
		sfpi := kit.PtrValue(sf).Interface()
		if f.Type.Kind() == reflect.Struct && f.Anonymous {
			n.CopyFieldsFrom(tfpi, sfpi)
		} else {
			switch {
			case sf.Kind() == reflect.Struct && kit.EmbeddedTypeImplements(sf.Type(), kitype):
				sfk := sfpi.(Ki)
				tfk := tfpi.(Ki)
				if tfk != nil && sfk != nil {
					tfk.CopyFrom(sfk)
				}
			case f.Type == KiT_Signal: // todo: don't copy signals by default
			case sf.Type().AssignableTo(tf.Type()):
				tf.Set(sf)
				// kit.PtrValue(tf).Set(sf)
			default:
				// use copier https://github.com/jinzhu/copier which handles as much as possible..
				copier.Copy(tfpi, sfpi)
			}
		}

	}
}

func (n *Node) CopyFromRaw(from Ki) error {
	n.CopyMakeChildrenFrom(from)
	n.DeleteAllProps(len(*from.Properties())) // start off fresh, allocated to size of from
	n.CopyPropsFrom(from, false)              // use shallow props copy by default
	n.CopyFieldsFrom(n.This(), from)
	for i, kid := range n.Kids {
		fmk := (*(from.Children()))[i]
		kid.CopyFromRaw(fmk)
	}
	return nil
}

func (n *Node) GetPtrPaths() {
	root := n.This()
	n.FuncDownMeFirst(0, root, func(k Ki, level int, d interface{}) bool {
		FlatFieldsValueFunc(k, func(stru interface{}, typ reflect.Type, field reflect.StructField, fieldVal reflect.Value) bool {
			if fieldVal.CanInterface() {
				vfi := kit.PtrValue(fieldVal).Interface()
				switch vfv := vfi.(type) {
				case *Ptr:
					vfv.GetPath()
					// case *Signal:
					// 	vfv.GetPaths()
				}
			}
			return true
		})
		return true
	})
}

func (n *Node) SetPtrsFmPaths() {
	root := n.Root()
	n.FuncDownMeFirst(0, root, func(k Ki, level int, d interface{}) bool {
		FlatFieldsValueFunc(k, func(stru interface{}, typ reflect.Type, field reflect.StructField, fieldVal reflect.Value) bool {
			if fieldVal.CanInterface() {
				vfi := kit.PtrValue(fieldVal).Interface()
				switch vfv := vfi.(type) {
				case *Ptr:
					if !vfv.PtrFmPath(root) {
						log.Printf("Ki Node SetPtrsFmPaths: could not find path: %v in root obj: %v", vfv.Path, root.Name())
					}
				}
			}
			return true
		})
		return true
	})
}

func (n *Node) UpdatePtrPaths(oldPath, newPath string, startOnly bool) {
	root := n.Root()
	n.FuncDownMeFirst(0, root, func(k Ki, level int, d interface{}) bool {
		FlatFieldsValueFunc(k, func(stru interface{}, typ reflect.Type, field reflect.StructField, fieldVal reflect.Value) bool {
			if fieldVal.CanInterface() {
				vfi := kit.PtrValue(fieldVal).Interface()
				switch vfv := vfi.(type) {
				case *Ptr:
					vfv.UpdatePath(oldPath, newPath, startOnly)
				}
			}
			return true
		})
		return true
	})
}

//////////////////////////////////////////////////////////////////////////
//  IO Marshal / Unmarshal support -- mostly in Slice

// see https://github.com/goki/ki/wiki/Naming for IO naming conventions

// Note: it is unfortunate that [Un]MarshalJSON uses byte[] instead of
// io.Reader / Writer..

// JSONTypePrefix is the first thing output in a ki tree JSON output file,
// specifying the type of the root node of the ki tree -- this info appears
// all on one { } bracketed line at the start of the file, and can also be
// used to identify the file as a ki tree JSON file
var JSONTypePrefix = []byte("{\"ki.RootType\": ")

// JSONTypeSuffix is just the } and \n at the end of the prefix line
var JSONTypeSuffix = []byte("}\n")

func (n *Node) WriteJSON(writer io.Writer, indent bool) error {
	err := n.ThisCheck()
	if err != nil {
		return err
	}
	var b []byte
	if indent {
		b, err = json.MarshalIndent(n.This(), "", "  ")
	} else {
		b, err = json.Marshal(n.This())
	}
	if err != nil {
		log.Println(err)
		return err
	}
	knm := kit.FullTypeName(n.Type())
	tstr := string(JSONTypePrefix) + fmt.Sprintf("\"%v\"}\n", knm)
	nwb := make([]byte, len(b)+len(tstr))
	copy(nwb, []byte(tstr))
	copy(nwb[len(tstr):], b) // is there a way to avoid this?
	_, err = writer.Write(nwb)
	if err != nil {
		log.Println(err)
		return err
	}
	return nil
}

func (n *Node) SaveJSON(filename string) error {
	fp, err := os.Create(filename)
	defer fp.Close()
	if err != nil {
		log.Println(err)
		return err
	}
	err = n.WriteJSON(fp, true) // use indent by default
	if err != nil {
		log.Println(err)
	}
	return err
}

func (n *Node) ReadJSON(reader io.Reader) error {
	err := n.ThisCheck()
	if err != nil {
		log.Println(err)
		return err
	}
	b, err := ioutil.ReadAll(reader)
	if err != nil {
		log.Println(err)
		return err
	}
	updt := n.UpdateStart()
	stidx := 0
	if bytes.HasPrefix(b, JSONTypePrefix) { // skip type
		stidx = bytes.Index(b, JSONTypeSuffix) + len(JSONTypeSuffix)
	}
	err = json.Unmarshal(b[stidx:], n.This()) // key use of this!
	if err == nil {
		n.UnmarshalPost()
	}
	n.SetFlag(int(ChildAdded)) // this might not be set..
	n.UpdateEnd(updt)
	return err
}

func (n *Node) OpenJSON(filename string) error {
	fp, err := os.Open(filename)
	defer fp.Close()
	if err != nil {
		log.Println(err)
		return err
	}
	return n.ReadJSON(fp)
}

// ReadNewJSON reads a new Ki tree from a JSON-encoded byte string, using type
// information at start of file to create an object of the proper type
func ReadNewJSON(reader io.Reader) (Ki, error) {
	b, err := ioutil.ReadAll(reader)
	if err != nil {
		log.Println(err)
		return nil, err
	}
	if bytes.HasPrefix(b, JSONTypePrefix) {
		stidx := len(JSONTypePrefix) + 1
		eidx := bytes.Index(b, JSONTypeSuffix)
		bodyidx := eidx + len(JSONTypeSuffix)
		tn := string(bytes.Trim(bytes.TrimSpace(b[stidx:eidx]), "\""))
		typ := kit.Types.Type(tn)
		if typ == nil {
			return nil, fmt.Errorf("ki.OpenNewJSON: kit.Types type name not found: %v", tn)
		}
		root := NewOfType(typ)
		root.Init(root)

		updt := root.UpdateStart()
		err = json.Unmarshal(b[bodyidx:], root)
		if err == nil {
			root.UnmarshalPost()
		}
		root.SetFlag(int(ChildAdded)) // this might not be set..
		root.UpdateEnd(updt)
		return root, nil
	} else {
		return nil, fmt.Errorf("ki.OpenNewJSON -- type prefix not found at start of file -- must be there to identify type of root node of tree\n")
	}
}

// OpenNewJSON opens a new Ki tree from a JSON-encoded file, using type
// information at start of file to create an object of the proper type
func OpenNewJSON(filename string) (Ki, error) {
	fp, err := os.Open(filename)
	defer fp.Close()
	if err != nil {
		log.Println(err)
		return nil, err
	}
	return ReadNewJSON(fp)
}

func (n *Node) WriteXML(writer io.Writer, indent bool) error {
	err := n.ThisCheck()
	if err != nil {
		log.Println(err)
		return err
	}
	var b []byte
	if indent {
		b, err = xml.MarshalIndent(n.This(), "", "  ")
	} else {
		b, err = xml.Marshal(n.This())
	}
	if err != nil {
		log.Println(err)
		return err
	}
	_, err = writer.Write(b)
	if err != nil {
		log.Println(err)
		return err
	}
	return nil
}

func (n *Node) ReadXML(reader io.Reader) error {
	var err error
	if err = n.ThisCheck(); err != nil {
		log.Println(err)
		return err
	}
	b, err := ioutil.ReadAll(reader)
	if err != nil {
		log.Println(err)
		return err
	}
	updt := n.UpdateStart()
	err = xml.Unmarshal(b, n.This()) // key use of this!
	if err == nil {
		n.UnmarshalPost()
	}
	n.UpdateEnd(updt)
	return nil
}

func (n *Node) ParentAllChildren() {
	n.FuncDownMeFirst(0, nil, func(k Ki, level int, d interface{}) bool {
		for _, child := range *k.Children() {
			if child != nil {
				child.SetParent(k)
			} else {
				return false
			}
		}
		return true
	})
}

func (n *Node) UnmarshalPost() {
	n.ParentAllChildren()
	n.SetPtrsFmPaths()
}

// Deleted manages all the deleted Ki elements, that are destined to then be
// destroyed, without having an additional pointer on the Ki object
type Deleted struct {
	Dels []Ki
	Mu   sync.Mutex
}

// DelMgr is the manager of all deleted items
var DelMgr = Deleted{}

// Add the Ki elements to the deleted list
func (dm *Deleted) Add(kis ...Ki) {
	dm.Mu.Lock()
	if dm.Dels == nil {
		dm.Dels = make([]Ki, 0, 1000)
	}
	dm.Dels = append(dm.Dels, kis...)
	dm.Mu.Unlock()
}

func (dm *Deleted) DestroyDeleted() {
	dm.Mu.Lock()
	curdels := make([]Ki, len(dm.Dels))
	copy(curdels, dm.Dels)
	dm.Dels = dm.Dels[:0]
	dm.Mu.Unlock()
	for _, k := range curdels {
		k.Destroy() // destroy will add to the dels so we need to do this outside of lock
	}
}