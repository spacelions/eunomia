package workflow

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ValueExpr represents either a typed literal value or an expr-lang/expr expression
// evaluated against Case State. Exactly one of Literal or Expr must be set.
type ValueExpr struct {
	literal    any
	hasLiteral bool
	expr       string
}

// LiteralExpr constructs a ValueExpr holding a static typed literal (including false, 0, or "").
func LiteralExpr(v any) ValueExpr {
	return ValueExpr{literal: v, hasLiteral: true}
}

// Expr constructs a ValueExpr holding an expr-lang/expr expression string.
func Expr(src string) ValueExpr {
	return ValueExpr{expr: src}
}

func (v ValueExpr) IsLiteral() bool { return v.hasLiteral }
func (v ValueExpr) IsExpr() bool    { return !v.hasLiteral && v.expr != "" }
func (v ValueExpr) Literal() any    { return v.literal }
func (v ValueExpr) Expr() string    { return v.expr }

// Validate enforces the strict XOR invariant between Literal and Expr.
func (v ValueExpr) Validate() error {
	hasExpr := strings.TrimSpace(v.expr) != ""
	if v.hasLiteral && hasExpr {
		return errors.New("value_expr must set either literal or expr, not both")
	}
	if !v.hasLiteral && !hasExpr {
		return errors.New("value_expr must set exactly one of literal or expr")
	}
	return nil
}

// MarshalJSON encodes ValueExpr as {"literal": ...} or {"expr": "..."}.
func (v ValueExpr) MarshalJSON() ([]byte, error) {
	if err := v.Validate(); err != nil {
		return nil, err
	}
	if v.hasLiteral {
		return json.Marshal(struct {
			Literal any `json:"literal"`
		}{Literal: v.literal})
	}
	return json.Marshal(struct {
		Expr string `json:"expr"`
	}{Expr: v.expr})
}

// UnmarshalJSON strictly decodes {"literal": ...} XOR {"expr": "..."}, rejecting unknown keys.
func (v *ValueExpr) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&raw); err != nil {
		return fmt.Errorf("decoding value_expr: %w", err)
	}
	if len(raw) == 0 {
		return errors.New("value_expr must set exactly one of literal or expr")
	}

	var out ValueExpr
	for k, val := range raw {
		switch k {
		case "literal":
			var lit any
			litDec := json.NewDecoder(bytes.NewReader(val))
			litDec.UseNumber()
			if err := litDec.Decode(&lit); err != nil {
				return fmt.Errorf("decoding value_expr literal: %w", err)
			}
			out.literal = lit
			out.hasLiteral = true
		case "expr":
			var src string
			if err := json.Unmarshal(val, &src); err != nil {
				return fmt.Errorf("decoding value_expr expr: %w", err)
			}
			out.expr = src
		default:
			return fmt.Errorf("unknown field %q in value_expr", k)
		}
	}

	if err := out.Validate(); err != nil {
		return err
	}
	*v = out
	return nil
}
