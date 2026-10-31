package tasmotacmd

// Tri is a three-valued truth: a condition about a build is only as known as the facts we have.
type Tri int8

// Truth values. The zero value is Unknown, so a forgotten fact never counts as "yes" or "no".
const (
	Unknown Tri = iota
	False
	True
)

// Not is the negation; Unknown stays Unknown.
func (t Tri) Not() Tri {
	switch t {
	case True:
		return False
	case False:
		return True
	case Unknown:
	}
	return Unknown
}

func and3(a, b Tri) Tri {
	if a == False || b == False {
		return False
	}
	if a == True && b == True {
		return True
	}
	return Unknown
}

func or3(a, b Tri) Tri {
	if a == True || b == True {
		return True
	}
	if a == False && b == False {
		return False
	}
	return Unknown
}

// EvalCond evaluates a preprocessor condition as it stands in Command.Requires: names (read as
// defined(NAME)), defined(NAME), defined NAME, !, && , || and parentheses. Anything else (a
// comparison, a macro call, __has_include) makes the whole condition Unknown: the catalogue does
// not evaluate what it cannot know. isDefined answers for one name.
func EvalCond(expr string, isDefined func(name string) Tri) Tri {
	p := &condParser{toks: tokenizeCond(expr), def: isDefined}
	if p.toks == nil {
		return Unknown
	}
	v := p.or()
	if p.bad || p.pos != len(p.toks) {
		return Unknown
	}
	return v
}

// CondNames lists the identifiers of a condition, without the operators and "defined".
func CondNames(expr string) []string {
	var out []string
	for _, t := range tokenizeCond(expr) {
		if isIdent(t) && t != "defined" {
			out = append(out, t)
		}
	}
	return out
}

func isIdent(t string) bool {
	c := t[0]
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// tokenizeCond returns nil for an expression with a token it does not understand.
func tokenizeCond(s string) []string {
	var out []string
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == ' ' || c == '\t':
			i++
		case c == '(' || c == ')' || c == '!' && (i+1 >= len(s) || s[i+1] != '='):
			out = append(out, string(c))
			i++
		case c == '&' && i+1 < len(s) && s[i+1] == '&', c == '|' && i+1 < len(s) && s[i+1] == '|':
			out = append(out, s[i:i+2])
			i += 2
		case c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9':
			j := i
			for j < len(s) && (s[j] == '_' || s[j] >= 'a' && s[j] <= 'z' || s[j] >= 'A' && s[j] <= 'Z' || s[j] >= '0' && s[j] <= '9') {
				j++
			}
			out = append(out, s[i:j])
			i = j
		default:
			return nil
		}
	}
	return out
}

type condParser struct {
	toks []string
	pos  int
	def  func(string) Tri
	bad  bool
}

func (p *condParser) peek() string {
	if p.pos < len(p.toks) {
		return p.toks[p.pos]
	}
	return ""
}

func (p *condParser) or() Tri {
	v := p.and()
	for p.peek() == "||" {
		p.pos++
		v = or3(v, p.and())
	}
	return v
}

func (p *condParser) and() Tri {
	v := p.unary()
	for p.peek() == "&&" {
		p.pos++
		v = and3(v, p.unary())
	}
	return v
}

func (p *condParser) unary() Tri {
	t := p.peek()
	switch {
	case t == "":
		p.bad = true
		return Unknown
	case t == "!":
		p.pos++
		return p.unary().Not()
	case t == "(":
		p.pos++
		v := p.or()
		if p.peek() != ")" {
			p.bad = true
			return Unknown
		}
		p.pos++
		return v
	case t == "defined":
		p.pos++
		paren := p.peek() == "("
		if paren {
			p.pos++
		}
		name := p.peek()
		if name == "" || !isIdent(name) {
			p.bad = true
			return Unknown
		}
		p.pos++
		if paren {
			if p.peek() != ")" {
				p.bad = true
				return Unknown
			}
			p.pos++
		}
		return p.def(name)
	case isIdent(t):
		p.pos++
		if p.peek() == "(" { // a macro call
			p.bad = true
			return Unknown
		}
		return p.def(t)
	}
	p.bad = true // a number or a stray token
	return Unknown
}
