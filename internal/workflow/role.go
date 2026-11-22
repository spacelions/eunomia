package workflow

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// RoleBinding is a dynamic participant reference resolved at runtime via the Actor Directory.
// Supported forms: "run.initiator", "workflow.owner", "role:<name>", "state.<field>".
// Raw emails and channel IDs are explicitly rejected.
type RoleBinding string

// RoleBindingKind identifies the resolution strategy for a RoleBinding.
type RoleBindingKind string

const (
	RoleBindingInitiator RoleBindingKind = "run.initiator"
	RoleBindingOwner     RoleBindingKind = "workflow.owner"
	RoleBindingRole      RoleBindingKind = "role"
	RoleBindingState     RoleBindingKind = "state"
)

var bindingIdentRe = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// Parse validates and splits a RoleBinding into its kind and target identifier.
func (rb RoleBinding) Parse() (RoleBindingKind, string, error) {
	raw := strings.TrimSpace(string(rb))
	if raw == "" {
		return "", "", errors.New("role binding must not be empty")
	}
	if strings.Contains(raw, "@") {
		return "", "", fmt.Errorf("role binding %q must not hardcode an email address; use run.initiator, workflow.owner, role:<name>, or state.<field>", raw)
	}

	switch raw {
	case string(RoleBindingInitiator):
		return RoleBindingInitiator, "", nil
	case string(RoleBindingOwner):
		return RoleBindingOwner, "", nil
	}

	if name, ok := strings.CutPrefix(raw, "role:"); ok {
		if !bindingIdentRe.MatchString(name) {
			return "", "", fmt.Errorf("role binding %q has invalid role name %q (must match ^[a-z][a-z0-9_]*$)", raw, name)
		}
		return RoleBindingRole, name, nil
	}

	if field, ok := strings.CutPrefix(raw, "state."); ok {
		if !bindingIdentRe.MatchString(field) {
			return "", "", fmt.Errorf("role binding %q has invalid state field %q (must match ^[a-z][a-z0-9_]*$)", raw, field)
		}
		return RoleBindingState, field, nil
	}

	return "", "", fmt.Errorf("invalid role binding %q (must be run.initiator, workflow.owner, role:<name>, or state.<field>)", raw)
}

// Validate checks that the RoleBinding conforms to one of the four allowed syntax forms.
func (rb RoleBinding) Validate() error {
	_, _, err := rb.Parse()
	return err
}
