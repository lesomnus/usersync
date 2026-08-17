package roster

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/parser"
	"github.com/goccy/go-yaml/token"
)

// Editing the roster in place, through the document's syntax tree.
//
// The obvious implementation — decode into Roster, change a field, re-encode —
// is wrong for this file, and measurably so. A round trip through the struct
// deletes every comment, deletes every blank line, expands `groups: [team-a]`
// into a two-line block sequence, collapses folded scalars onto one line,
// re-quotes prose that happens to contain ": " or " - ", and reorders keys into
// struct order. On the shipped roster.yaml that is 17 of 46 lines rewritten.
//
// None of that is data loss, and all of it is the difference between a change
// an operator can review and a diff that says the whole file changed. The
// roster's history is the account-management record; a record nobody reads
// because every entry is a full-file rewrite is not one.
//
// So edits are made to the parsed AST and the document is printed back. Adding
// a member to a team touches exactly the line that says who is on that team.

// ErrNoSuchUser is returned when the roster does not declare the name.
var ErrNoSuchUser = errors.New("roster: no such user")

// ErrNoSuchGroup is returned when the roster does not declare the group.
var ErrNoSuchGroup = errors.New("roster: no such group")

// ErrNotEditable is returned when the file's shape is one this editor will not
// touch. It is a refusal, not a failure: the roster is hand-editable by design,
// and a structure the editor does not recognise is far more likely to be
// something a person meant than something to be rewritten.
var ErrNotEditable = errors.New("roster: cannot edit this file safely")

// Document is a parsed roster that can be edited and printed back.
type Document struct {
	file *ast.File
	// groups is the sequence node under the top-level `groups:` key, where
	// membership is declared and edited (`groups[].members`).
	groups *ast.SequenceNode
}

// ParseDocument parses YAML while retaining comments and layout.
func ParseDocument(src []byte) (*Document, error) {
	f, err := parser.ParseBytes(src, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("roster: parse: %w", err)
	}
	if len(f.Docs) != 1 {
		return nil, fmt.Errorf("%w: expected one YAML document, found %d", ErrNotEditable, len(f.Docs))
	}
	body, ok := f.Docs[0].Body.(*ast.MappingNode)
	if !ok {
		return nil, fmt.Errorf("%w: the document is not a mapping", ErrNotEditable)
	}

	d := &Document{file: f}
	for _, kv := range body.Values {
		if kv.Key.GetToken().Value != "groups" {
			continue
		}
		seq, ok := kv.Value.(*ast.SequenceNode)
		if !ok {
			return nil, fmt.Errorf("%w: `groups` is not a sequence", ErrNotEditable)
		}
		d.groups = seq
	}
	if d.groups == nil {
		return nil, fmt.Errorf("%w: no `groups` sequence", ErrNotEditable)
	}
	return d, nil
}

// String prints the document.
func (d *Document) String() string { return d.file.String() }

// groupNode finds the mapping for one group by name.
func (d *Document) groupNode(name string) (*ast.MappingNode, error) {
	for _, entry := range d.groups.Values {
		m, ok := entry.(*ast.MappingNode)
		if !ok {
			continue
		}
		for _, kv := range m.Values {
			if kv.Key.GetToken().Value == "name" && kv.Value.GetToken().Value == name {
				return m, nil
			}
		}
	}
	return nil, fmt.Errorf("%w: %q", ErrNoSuchGroup, name)
}

// Members reads a group's declared members.
func (d *Document) Members(group string) ([]string, error) {
	m, err := d.groupNode(group)
	if err != nil {
		return nil, err
	}
	_, values, err := membersOf(m)
	return values, err
}

// membersOf returns the `members:` key/value pair and the names in it. A group
// with no `members:` key has none, which is not an error — it is how an empty
// team (or an `all: true` group) is written.
func membersOf(m *ast.MappingNode) (*ast.MappingValueNode, []string, error) {
	for _, kv := range m.Values {
		if kv.Key.GetToken().Value != "members" {
			continue
		}
		seq, ok := kv.Value.(*ast.SequenceNode)
		if !ok {
			return nil, nil, fmt.Errorf("%w: `members` is not a sequence", ErrNotEditable)
		}
		names := make([]string, 0, len(seq.Values))
		for _, v := range seq.Values {
			names = append(names, v.GetToken().Value)
		}
		return kv, names, nil
	}
	return nil, nil, nil
}

// SetMembers replaces a group's member list.
//
// The rendered style follows what is already there: a flow sequence stays a flow
// sequence. That is not cosmetic — the roster is written `members: [alice, bob]`
// and silently converting it to a block sequence on the first membership change
// is the kind of churn this whole file exists to avoid.
//
// A group with no `members:` key gets one appended after the last key, which is
// where a person would have put it.
func (d *Document) SetMembers(group string, members []string) error {
	m, err := d.groupNode(group)
	if err != nil {
		return err
	}
	kv, _, err := membersOf(m)
	if err != nil {
		return err
	}

	if len(members) == 0 && kv != nil {
		// Drop the key entirely rather than leaving `members: []`. An empty list
		// and an absent key mean the same thing to the reconciler, and the absent
		// key is what a hand-written empty team looks like.
		m.Values = slices.DeleteFunc(m.Values, func(v *ast.MappingValueNode) bool { return v == kv })
		return nil
	}
	if len(members) == 0 {
		return nil
	}

	if kv == nil {
		node, err := newMembersPair(m, members, true)
		if err != nil {
			return err
		}
		m.Values = append(m.Values, node)
		return nil
	}

	seq := kv.Value.(*ast.SequenceNode)
	flow := seq.IsFlowStyle
	values := make([]ast.Node, 0, len(members))
	for _, name := range members {
		v, err := stringNode(name, seq.GetToken())
		if err != nil {
			return err
		}
		values = append(values, v)
	}
	seq.Values = values
	seq.IsFlowStyle = flow
	return nil
}

// AddMember adds a user to a group, idempotently.
func (d *Document) AddMember(group, user string) (changed bool, err error) {
	have, err := d.Members(group)
	if err != nil {
		return false, err
	}
	if slices.Contains(have, user) {
		return false, nil
	}
	next := append(slices.Clone(have), user)
	slices.Sort(next)
	return true, d.SetMembers(group, next)
}

// RemoveMember removes a user from a group, idempotently.
func (d *Document) RemoveMember(group, user string) (changed bool, err error) {
	have, err := d.Members(group)
	if err != nil {
		return false, err
	}
	if !slices.Contains(have, user) {
		return false, nil
	}
	next := slices.DeleteFunc(slices.Clone(have), func(u string) bool { return u == user })
	return true, d.SetMembers(group, next)
}

// newMembersPair builds a `members: [a, b]` mapping value to append to a group.
func newMembersPair(m *ast.MappingNode, members []string, flow bool) (*ast.MappingValueNode, error) {
	if len(m.Values) == 0 {
		return nil, fmt.Errorf("%w: group entry has no keys", ErrNotEditable)
	}
	// Borrow position from the entry's last key so the new line is indented with
	// its siblings rather than at column zero.
	anchor := m.Values[len(m.Values)-1]
	tk := anchor.Key.GetToken()

	key, err := stringNode("members", tk)
	if err != nil {
		return nil, err
	}
	seq := ast.Sequence(tokenAt(tk, "["), flow)
	for _, name := range members {
		v, err := stringNode(name, tk)
		if err != nil {
			return nil, err
		}
		seq.Values = append(seq.Values, v)
	}
	return ast.MappingValue(tokenAt(tk, ":"), key, seq), nil
}

func stringNode(s string, at *token.Token) (*ast.StringNode, error) {
	return ast.String(tokenAt(at, s)), nil
}

func tokenAt(at *token.Token, value string) *token.Token {
	tk := token.New(value, value, at.Position)
	// Same line and column as the anchor: the printer lays the document out from
	// the nodes it already has, and a zero position would put this at the margin.
	pos := *at.Position
	tk.Position = &pos
	return tk
}

// WriteFile writes src to path atomically, preserving the file's existing mode.
//
// Atomic because the reader is a boot sequence that refuses to start on an
// unparseable roster: a torn write would not corrupt data, it would prevent the
// server from coming up. Mode-preserving because os.CreateTemp makes 0600 files
// and the roster is mounted for other readers.
func WriteFile(path string, src []byte) error {
	mode := os.FileMode(0o644)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}

	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".roster-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	cleanup := func() { f.Close(); os.Remove(tmp) }

	if _, err := f.Write(src); err != nil {
		cleanup()
		return err
	}
	// fsync before the rename: rename is atomic with respect to readers, but
	// without this the rename can land in the journal ahead of the bytes and a
	// power loss leaves a zero-length roster -- which is a file that parses to
	// nothing and takes every account with it.
	if err := f.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := f.Chmod(mode); err != nil {
		cleanup()
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
