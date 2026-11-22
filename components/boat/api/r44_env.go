package main

// baseEnvName is boat.dev's name for the default environment. It is only a
// back-compat fallback: a team's own environment named "base" is a real name
// (resolveEnvRef finds it first).
const baseEnvName = "base"

// resolveEnvRef is the one resolver of an environment reference on every path
// that takes one (create, start, fork, clone; native and compat):
//   - "" (omitted or empty): the team default (nil, found, when the team has none:
//     no vars, the default selection);
//   - anything else is matched first as an environment id, then as an exact name;
//   - "base" with no environment literally named "base" (or with that id) is the
//     team default, as before;
//   - any other value is unknown (found false).
func resolveEnvRef(st *State, team, name string) (env *Environment, found bool) {
	if name == "" {
		return defaultEnv(st, team), true
	}
	if e := st.Environments[name]; e != nil && e.TeamID == team {
		return e, true
	}
	if e := envByName(st, team, name); e != nil {
		return e, true
	}
	if name == baseEnvName {
		return defaultEnv(st, team), true
	}
	return nil, false
}
