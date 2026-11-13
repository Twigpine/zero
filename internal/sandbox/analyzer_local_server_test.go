package sandbox

import "testing"

// LocalServer exists to say one narrow thing: this command BINDS a local port,
// so it is not egress and should not be treated as network access. That claim
// is only worth anything if it tracks what the command actually does.
//
// Matching on the program name alone made every invocation of a framework CLI a
// "server", so `next build` and `vite build` claimed to bind a port while
// compiling to disk. Nothing reads the flag today, which is exactly why it could
// be wrong quietly; the first reader would have inherited the bug.
func TestLocalServerDistinguishesServingFromBuilding(t *testing.T) {
	serving := []string{
		"next dev", "next start", "nuxt dev", "astro dev", "astro preview",
		"vite", "vite serve", "vite preview",
		// Options only, no subcommand. firstSubcommand skips the flag but not the
		// value it consumes, so this used to resolve to "127.0.0.1" and read as a
		// build.
		"vite --host 127.0.0.1", "vite --port 5173",
		// Dedicated servers, whose entire purpose is to bind.
		"http-server", "http-server ./public", "serve", "serve dist",
		// Via a package manager, the pre-existing path.
		"npm run dev", "pnpm start", "yarn preview",
	}
	for _, command := range serving {
		if !AnalyzeCommand(command).LocalServer {
			t.Errorf("%q does bind a local port but was not classified as a local server", command)
		}
	}

	building := []string{
		"next build", "next lint", "next",
		"vite build", "vite optimize",
		"nuxt generate", "nuxt build",
		"astro check", "astro build",
		"npm run build",
	}
	for _, command := range building {
		if AnalyzeCommand(command).LocalServer {
			t.Errorf("%q compiles rather than serving but was classified as a local server", command)
		}
	}
}

// BUILDING IS NOT SERVING, BUT IT IS STILL NETWORK.
//
// The serve/build distinction lives in LocalServer alone. Network stays
// conservative for the whole framework inventory, because next, vite, nuxt and
// astro run the repository config, plugins and application code during a build
// as surely as during a serve, and the subcommand name cannot prove that code
// makes no requests. An earlier head of this branch let build-like spellings
// drop to Network=false, and on Windows the approval gate IS the network
// boundary, so an ordinary shell grant would have run them without the separate
// network decision main applies. Reported by gnanam1990.
func TestFrameworkBuildsStayNetworkWithoutBeingServers(t *testing.T) {
	for _, command := range []string{
		"next build", "next lint", "next",
		"vite build", "vite optimize",
		"nuxt generate", "nuxt build",
		"astro check", "astro build",
		// Option-bearing forms: a more precise action parse must not remove the gate.
		"next build --profile", "vite build --mode production",
		"vite --config vite.prod.ts build", "nuxt generate --dotenv .env.prod",
		"astro check --watch",
	} {
		analysis := AnalyzeCommand(command)
		if !analysis.Network {
			t.Errorf("%q lost its network classification; a framework build runs repository code and cannot be proven inert", command)
		}
		if analysis.LocalServer {
			t.Errorf("%q compiles rather than serving but was classified as a local server", command)
		}
	}
	for _, command := range []string{"next dev", "vite", "nuxt dev", "astro dev", "astro preview"} {
		analysis := AnalyzeCommand(command)
		if !analysis.Network || !analysis.LocalServer {
			t.Errorf("%q = Network %v LocalServer %v, want both: serving binds AND may egress", command, analysis.Network, analysis.LocalServer)
		}
	}
}

// SERVING KEEPS ITS NETWORK APPROVAL until something consumes LocalServer.
//
// The classification alone grants nothing: no policy or runner code reads
// LocalServer, so treating a serving command as local-only only removed the
// approval it used to get, and the command then ran under the default deny
// profile. On Linux that is a network namespace and on macOS it is
// (deny network*), so the server started unprompted and could not serve a
// preview to the operator's browser.
//
// The package-manager cases are the sharper half. "npm run dev" is matched by
// SCRIPT NAME, and the repository decides what "dev" and "predev" actually do;
// either can curl before anything binds a port. On Windows the approval gate IS
// the network protection, so inferring no-egress from a name lets that through
// unprompted.
func TestServingStillRequiresNetworkApproval(t *testing.T) {
	for _, command := range []string{"next dev", "vite", "http-server", "npm run dev", "pnpm dev", "python -m http.server"} {
		analysis := AnalyzeCommand(command)
		if !analysis.LocalServer {
			t.Errorf("%q was not recognised as a local server", command)
		}
		if !analysis.Network {
			t.Errorf("%q lost its network approval, so it runs under the deny profile and cannot serve a preview", command)
		}
	}
}

// A GLOBAL OPTION BEFORE THE SUBCOMMAND MUST NOT HIDE IT.
//
// The subcommand scan skips flags but not the word they CONSUME, so
// `npm --prefix ./web run dev` resolved to "./web" and the dev server stopped
// being recognised at all — it lost the network approval its unflagged spelling
// gets, and on Windows that approval IS the egress control. A global option
// ahead of the subcommand is documented usage for every one of these tools
// (`npm --prefix`, `pnpm -C`, `yarn --cwd`, `bun --cwd`), not a contrived form.
//
// Driven off packageManagerServingScripts rather than a written-out list, so a
// script name added there is covered in the flagged spelling too instead of only
// in the one someone remembered to type.
func TestPackageManagerOptionValuesDoNotHideTheServingSubcommand(t *testing.T) {
	if len(packageManagerServingScripts) == 0 {
		t.Fatal("SETUP INVALID: no serving scripts to check")
	}
	for _, manager := range []struct{ prog, option string }{
		{"npm", "--prefix"},
		{"pnpm", "-C"},
		{"yarn", "--cwd"},
		{"bun", "--cwd"},
	} {
		for script := range packageManagerServingScripts {
			for _, command := range []string{
				manager.prog + " " + manager.option + " ./web " + script,
				manager.prog + " " + manager.option + " ./web run " + script,
			} {
				t.Run(command, func(t *testing.T) {
					analysis := AnalyzeCommand(command)
					if analysis.TooComplex {
						t.Fatalf("SETUP INVALID: %q was expected to parse", command)
					}
					if !analysis.LocalServer {
						t.Errorf("%q binds a local port, but the option's value was read as its subcommand", command)
					}
					if !analysis.Network {
						t.Errorf("%q lost the network approval its unflagged spelling receives", command)
					}
				})
			}
		}
	}
}
