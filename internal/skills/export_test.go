// Test seams: helpers only test code uses, kept out of the production binary.
package skills

import "strings"

// Info returns the named skill plus its recorded source and hash, or ok=false if
// it is not discoverable in dir.
func Info(dir string, name string) (SkillInfo, bool) {
	skill, ok := Get(dir, name)
	if !ok {
		return SkillInfo{}, false
	}
	info := SkillInfo{Skill: skill}
	if lock, err := ReadLock(dir); err == nil {
		if entry, found := lock[skill.Name]; found {
			info.Source = entry.Source
			info.Hash = entry.Hash
			info.TreeHash = entry.TreeHash
		}
	}
	info.HashDrift = skillHashDrift(skill, info.Hash, info.TreeHash)
	return info, true
}

// DiscoveryRoots returns ordered skill roots for runtime discovery: primary
// DefaultDir, optional AgentsDir when present, then pluginRoots. Empty strings
// are omitted. Earlier entries win on name clashes.
func DiscoveryRoots(env map[string]string, pluginRoots []string) []string {
	return collectRoots(DefaultDir(env), AgentsDir(env), pluginRoots)
}

// Duplicates returns the duplicate-name collisions Load resolved by the
// first-directory-wins rule, so a caller can warn the user that a shadowed skill
// was dropped. A missing directory yields no duplicates and no error.
func Duplicates(dir string) ([]DuplicateName, error) {
	_, dups, err := load(dir)
	return dups, err
}

// Get loads the named skill from dir, returning false if it is not found.
func Get(dir string, name string) (Skill, bool) {
	loaded, err := Load(dir)
	if err != nil {
		return Skill{}, false
	}
	target := strings.TrimSpace(name)
	for _, skill := range loaded {
		if skill.Name == target {
			return skill, true
		}
	}
	return Skill{}, false
}

// List loads the skills directory and returns each skill without its (possibly
// large) Content body — handy for `zero skills` listings.
func List(dir string) ([]Skill, error) {
	loaded, err := Load(dir)
	if err != nil {
		return nil, err
	}
	listed := make([]Skill, 0, len(loaded))
	for _, skill := range loaded {
		skill.Content = ""
		listed = append(listed, skill)
	}
	return listed, nil
}
