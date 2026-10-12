# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project
aims to follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html) once the first release is
tagged. Until then, source builds report the version `dev`.

## [0.9.0](https://github.com/Gitlawb/zero/compare/v0.8.0...v0.9.0) (2026-09-15)


### Features

* **acp:** add standard session list and resume ([#914](https://github.com/Gitlawb/zero/issues/914)) ([c1937df](https://github.com/Gitlawb/zero/commit/c1937dfac72e6ad0e5ade6e48e2d9c17d9c3e5d6))
* **acp:** emit structured file changes in tool updates ([#999](https://github.com/Gitlawb/zero/issues/999)) ([faec8ee](https://github.com/Gitlawb/zero/commit/faec8ee2017fdd6d0c2c347add892cb73eb413d7))
* **acp:** surface safe browser tool metadata ([#1000](https://github.com/Gitlawb/zero/issues/1000)) ([a6ec043](https://github.com/Gitlawb/zero/commit/a6ec0435e36b9bea9793b2fd97f2ecd30e3a0f9b))
* **exec:** default headless runs to the turn-budget ceiling ([#1049](https://github.com/Gitlawb/zero/issues/1049)) ([9e609c5](https://github.com/Gitlawb/zero/commit/9e609c52420fd322f047fc238c04ac81dd623fdd))
* **tui:** interactive terminal attach for tty exec sessions ([#1055](https://github.com/Gitlawb/zero/issues/1055)) ([d73ec81](https://github.com/Gitlawb/zero/commit/d73ec81e2d3facb5377bc9ad0fe2aa314628a0c0))
* **tui:** offer mid-run model escalation behind --allow-escalation ([#1019](https://github.com/Gitlawb/zero/issues/1019)) ([91d921f](https://github.com/Gitlawb/zero/commit/91d921f4c5b8289a9e437f786218f1b48f2add5a))
* **tui:** richer /resume session browser ([#1047](https://github.com/Gitlawb/zero/issues/1047)) ([2f53117](https://github.com/Gitlawb/zero/commit/2f531178f614e587b629e4f81e0470adda647fbd))


### Bug Fixes

* **acp:** make configured MCP tools callable ([#1003](https://github.com/Gitlawb/zero/issues/1003)) ([7f5e5af](https://github.com/Gitlawb/zero/commit/7f5e5af7bbdbe5bb4d91706146072d461c0234a4))
* **agent:** stop the tool-schema ratchet meaning a different thing per host ([#1017](https://github.com/Gitlawb/zero/issues/1017)) ([f909266](https://github.com/Gitlawb/zero/commit/f9092668a832ea73a69551f0ea2df4f6eaf356df))
* **background:** treat POSIX zombie leader as already-exited ([#981](https://github.com/Gitlawb/zero/issues/981)) ([1aacf38](https://github.com/Gitlawb/zero/commit/1aacf38b727d31b5ca09949316d6afa8292aec46))
* **config:** lock config read-modify-write across processes ([#960](https://github.com/Gitlawb/zero/issues/960)) ([b04fe96](https://github.com/Gitlawb/zero/commit/b04fe96e9ef3f2deda4fb2a972270300629079d2))
* **config:** preserve unknown top-level fields ([#984](https://github.com/Gitlawb/zero/issues/984)) ([488def6](https://github.com/Gitlawb/zero/commit/488def68dcc6d36701fdf171e94a74a436b090fc))
* **daemon:** publish status files atomically ([#949](https://github.com/Gitlawb/zero/issues/949)) ([9001421](https://github.com/Gitlawb/zero/commit/90014210d2ada2e3d61f4eca5087d3f35a3c4b8b))
* **installtxn:** put back an install a killed commit left behind ([#997](https://github.com/Gitlawb/zero/issues/997)) ([2837f5e](https://github.com/Gitlawb/zero/commit/2837f5efb38dcd3c69771b0ba31565f98a206120))
* let the configured forward proxy through the loopback guard ([#1025](https://github.com/Gitlawb/zero/issues/1025)) ([1e4db7c](https://github.com/Gitlawb/zero/commit/1e4db7c6643ca7e2c3d8e4b8b30371cfbe43561e))
* **lockutil:** use kernel-held advisory locks ([#950](https://github.com/Gitlawb/zero/issues/950)) ([6fe0d1e](https://github.com/Gitlawb/zero/commit/6fe0d1edcdbadea6d566800fbcca739c1cd54ea1))
* **mcp:** discover OAuth through protected resource metadata ([#991](https://github.com/Gitlawb/zero/issues/991)) ([1b5db17](https://github.com/Gitlawb/zero/commit/1b5db1765672820caac1684b168c9898b5ba3593))
* **mcp:** reject a server-initiated request as a streamable-HTTP response ([#1054](https://github.com/Gitlawb/zero/issues/1054)) ([2479d94](https://github.com/Gitlawb/zero/commit/2479d946c15dbf74ee3c04a637e97181d1786f08))
* **oauth:** force-close loopback callback connections past the shutdown deadline ([#1051](https://github.com/Gitlawb/zero/issues/1051)) ([176f5a2](https://github.com/Gitlawb/zero/commit/176f5a242b754a86ea4c7121a8dcc1c0d72cf2e4))
* **oauth:** Split an oversized keyring token blob across entries ([#1007](https://github.com/Gitlawb/zero/issues/1007)) ([275e248](https://github.com/Gitlawb/zero/commit/275e2486695338ce38193a6e3319f8b6a9a5c33c))
* **providerio:** close idle connections periodically to prevent HTTP/2 PROTOCOL_ERROR ([#1058](https://github.com/Gitlawb/zero/issues/1058)) ([b76c764](https://github.com/Gitlawb/zero/commit/b76c764b77926891aa67163e6605a2d8540aacaa))
* **providers:** register Atomic Chat as local OpenAI-compatible runtime ([#550](https://github.com/Gitlawb/zero/issues/550)) ([79fa142](https://github.com/Gitlawb/zero/commit/79fa1422ef1a3691b93342788453ebc284d57f9c))
* **redaction:** split camelCase keys in normalizeKey ([#994](https://github.com/Gitlawb/zero/issues/994)) ([08fbea7](https://github.com/Gitlawb/zero/commit/08fbea7a3221d2061d88038924f90a4cba9af1ac))
* **sandbox:** keep a derived ACL target inside the write root it came from ([#1040](https://github.com/Gitlawb/zero/issues/1040)) ([3b43c43](https://github.com/Gitlawb/zero/commit/3b43c43eca968c8619adadc81fcc1a17e220216a))
* **sandbox:** keep Windows restricted-token SIDs narrow and fail closed on DenyRead ([#1006](https://github.com/Gitlawb/zero/issues/1006)) ([0207f2b](https://github.com/Gitlawb/zero/commit/0207f2b2697149a73ecc89a5ab5db0bd665f1911))
* **sandbox:** Make WSL backend tests deterministic. ([#948](https://github.com/Gitlawb/zero/issues/948)) ([e59bf78](https://github.com/Gitlawb/zero/commit/e59bf78cabc2703678778b904c27c8bc8ef82bc8))
* **sandbox:** protect worktree git pointers ([#805](https://github.com/Gitlawb/zero/issues/805)) ([f30f550](https://github.com/Gitlawb/zero/commit/f30f550e6037c53f8521ba2d59c984ae6c574917))
* **sandbox:** refcount temporary roots so one holder's cleanup cannot revoke another's ([#911](https://github.com/Gitlawb/zero/issues/911)) ([7acac7a](https://github.com/Gitlawb/zero/commit/7acac7acee65565e25089f2a7b5813c086ce65eb))
* **sessions:** carry an interrupted turn's work into the resume prompt ([#1016](https://github.com/Gitlawb/zero/issues/1016)) ([6ae8396](https://github.com/Gitlawb/zero/commit/6ae8396f0282a5f18a431f0c0ef1a272ecc2ca2b))
* **sessions:** render resume context fields in key order ([#1043](https://github.com/Gitlawb/zero/issues/1043)) ([da244bc](https://github.com/Gitlawb/zero/commit/da244bcbfcfa05e414567ac0acc3cf8ba745ad7d))
* **specialist:** keep the pinned model when a specialist is resumed ([#1009](https://github.com/Gitlawb/zero/issues/1009)) ([aadb4a2](https://github.com/Gitlawb/zero/commit/aadb4a27e9cdb41e774e8e1a8f23cf9c2fb50db2))
* **swarm:** stop original member before handoff ([#957](https://github.com/Gitlawb/zero/issues/957)) ([0021475](https://github.com/Gitlawb/zero/commit/0021475d4c58c91911098f414795512cf60a0b56))
* **tools:** an edit keeps the reads it did not disturb ([#908](https://github.com/Gitlawb/zero/issues/908)) ([1649008](https://github.com/Gitlawb/zero/commit/1649008506da0e675431dc056251a01891b15cc8))
* **tools:** make apply_patch tolerant and re-expose edit_file ([#956](https://github.com/Gitlawb/zero/issues/956)) ([2749447](https://github.com/Gitlawb/zero/commit/2749447cdce4ae5dd333538b36fa919b80ecc66d))
* **tools:** reject fuzzy edits with drifted content ([#983](https://github.com/Gitlawb/zero/issues/983)) ([8b3d1b1](https://github.com/Gitlawb/zero/commit/8b3d1b1597b04bfa6d4bc7f912abc13ca80a6298))
* **tools:** say when format-on-write ran out of time ([#1044](https://github.com/Gitlawb/zero/issues/1044)) ([6937a30](https://github.com/Gitlawb/zero/commit/6937a309cf00825572210a7610a1f3ea8b74c2f9))
* **tui:** move voice capture to Ctrl+Space ([#968](https://github.com/Gitlawb/zero/issues/968)) ([eeea330](https://github.com/Gitlawb/zero/commit/eeea3308fed38b37b203522624babaf2f30df91f))
* **update:** keep Windows failed-restore state in trusted recovery records ([#979](https://github.com/Gitlawb/zero/issues/979)) ([a3852ba](https://github.com/Gitlawb/zero/commit/a3852bac595e06c5d3ca9da22212b281b8796802))


### Performance Improvements

* **agent:** cheap compaction — dedicated summarizer model, input cap… ([#532](https://github.com/Gitlawb/zero/issues/532)) ([6645f97](https://github.com/Gitlawb/zero/commit/6645f9744b37dda4d52ecc4631578d62cefc89de))
* **providers:** parallelize isolated provider unit tests and shrink retry backoffs ([#955](https://github.com/Gitlawb/zero/issues/955)) ([d6b25fc](https://github.com/Gitlawb/zero/commit/d6b25fc3039a6b7d72a5703a87a097449792b102))
* reduce startup and turn overhead ([#932](https://github.com/Gitlawb/zero/issues/932)) ([1fde941](https://github.com/Gitlawb/zero/commit/1fde941803c4295c11a4a14559f3c5217d1a9e69))

## [0.8.0](https://github.com/Gitlawb/zero/compare/v0.7.0...v0.8.0) (2026-08-21)


### Features

* **security:** path-jail primitive, credential-store locking, worktree git hardening ([#891](https://github.com/Gitlawb/zero/issues/891)) ([060d38c](https://github.com/Gitlawb/zero/commit/060d38ce716feb7c5b6657b082cd35cb8dda3d75))
* **update:** recognise Homebrew installs and add mise, go install docs ([#910](https://github.com/Gitlawb/zero/issues/910)) ([01c3498](https://github.com/Gitlawb/zero/commit/01c3498205e69d26e5f929225f9455b276444f39))
* use Exa as default search provider ([#926](https://github.com/Gitlawb/zero/issues/926)) ([1ec7219](https://github.com/Gitlawb/zero/commit/1ec7219aee7b2840481ab0905f131644e27d2a4e))
* **zerogit:** auto-create a conventional branch before push/pr on default branch ([#671](https://github.com/Gitlawb/zero/issues/671)) ([d065467](https://github.com/Gitlawb/zero/commit/d065467cd3ec3ed91c5dc60554c36fc4b4d6de4c))


### Bug Fixes

* **acp:** three defects a desktop ACP client hits ([#915](https://github.com/Gitlawb/zero/issues/915)) ([a78c6c3](https://github.com/Gitlawb/zero/commit/a78c6c357e1d21146d7cd2e8030a754e06f7f1f5))
* bump Go to 1.26.6 for stdlib vulnerability fixes ([#903](https://github.com/Gitlawb/zero/issues/903)) ([dc15e82](https://github.com/Gitlawb/zero/commit/dc15e822987d8d7de467c62c375518f435b74d8c))
* **daemon:** clean up child after startup timeout ([#774](https://github.com/Gitlawb/zero/issues/774)) ([9c29209](https://github.com/Gitlawb/zero/commit/9c292092a26c9092fd873d072b0af1b0d9d39ef0))
* **dictation:** redact API keys from streaming transcriber errors ([#852](https://github.com/Gitlawb/zero/issues/852)) ([64a783b](https://github.com/Gitlawb/zero/commit/64a783b08caa2c7b21f188033b9ed84a14e00e84))
* **modelregistry:** expose reasoning effort for DeepSeek V4 models ([#931](https://github.com/Gitlawb/zero/issues/931)) ([90dcfd1](https://github.com/Gitlawb/zero/commit/90dcfd127e8a6d9902ec9f4e72f6d03fef1a0fc6))
* **providers:** discover ChatGPT capabilities ([#890](https://github.com/Gitlawb/zero/issues/890)) ([2d2450e](https://github.com/Gitlawb/zero/commit/2d2450e9a744349f0d01b1d4e9ba29c24ba5650d))
* **sandbox:** normalize launcher names before the command-prefix denylist ([#934](https://github.com/Gitlawb/zero/issues/934)) ([6edf9a8](https://github.com/Gitlawb/zero/commit/6edf9a8b78dc030dc44598919db1c7fa9d4f809a))

## [0.7.0](https://github.com/Gitlawb/zero/compare/v0.6.0...v0.7.0) (2026-08-10)


### Features

* **agent:** add PermissionModePlan for interactive read-only planning ([#853](https://github.com/Gitlawb/zero/issues/853)) ([0176ee4](https://github.com/Gitlawb/zero/commit/0176ee49ffddcff6f7edf6ae2ade4967e5b22cc3))
* **providers:** live model lists for OpenRouter and OpenGateway ([#860](https://github.com/Gitlawb/zero/issues/860)) ([fb4f293](https://github.com/Gitlawb/zero/commit/fb4f293c20eb860ca388a10d22fd566ce282ba1f))
* **tools:** give tool results an image channel and add view_image ([#843](https://github.com/Gitlawb/zero/issues/843)) ([cd0eb19](https://github.com/Gitlawb/zero/commit/cd0eb194e4df4b844785796c0701216740a56344))
* **tui:** replace /retitle with local /rename ([#826](https://github.com/Gitlawb/zero/issues/826)) ([5886354](https://github.com/Gitlawb/zero/commit/588635487f22ef4d62d350140a219b7b8bb24119))


### Bug Fixes

* **agent:** end the additional_permissions retry loop ([#864](https://github.com/Gitlawb/zero/issues/864)) ([4ca4fa7](https://github.com/Gitlawb/zero/commit/4ca4fa75d31b139dbe55a3badf17da86892ab001))
* **agent:** tell the user a prefix approval leaves the sandbox ([#885](https://github.com/Gitlawb/zero/issues/885)) ([2ec5c3a](https://github.com/Gitlawb/zero/commit/2ec5c3ae6b8fb543887bdeafdb34c4da057b8d36))
* **cli:** report MCP servers that failed to start ([#822](https://github.com/Gitlawb/zero/issues/822)) ([#827](https://github.com/Gitlawb/zero/issues/827)) ([021281e](https://github.com/Gitlawb/zero/commit/021281ebbab3f2bbe6a77b2dab4bf121bcfe6b52))
* **lsp:** dispatch notifications off read loop ([#759](https://github.com/Gitlawb/zero/issues/759)) ([936aa4e](https://github.com/Gitlawb/zero/commit/936aa4e77cf49738560a89395b8ba4d22263a396))
* **mcp:** name the non-text blocks a tool result drops ([#874](https://github.com/Gitlawb/zero/issues/874)) ([027a670](https://github.com/Gitlawb/zero/commit/027a67080bea511b876c284bc911ef011eceb308))
* **sandbox:** deny reads of Zero credential stores ([#681](https://github.com/Gitlawb/zero/issues/681)) ([55fa05f](https://github.com/Gitlawb/zero/commit/55fa05f0ab5a01b495345d43464606cd19a31b4f))
* **sandbox:** stop the Windows write jail honouring Everyone-granted paths ([#865](https://github.com/Gitlawb/zero/issues/865)) ([91b413c](https://github.com/Gitlawb/zero/commit/91b413c514cc1607d5b4e7936780d40569239814))
* **secrets,worktrees:** fix secret redaction leakage and prune stale worktrees ([#855](https://github.com/Gitlawb/zero/issues/855)) ([ff62f73](https://github.com/Gitlawb/zero/commit/ff62f73b7f4dc5e2472db21acba43cf6140c06d1))
* **specialist:** make overwrites atomic ([#757](https://github.com/Gitlawb/zero/issues/757)) ([30f6d8f](https://github.com/Gitlawb/zero/commit/30f6d8f591a0b81638b4182ff56c86c8a50a5c70))
* suppress gosec G204 warning for trusted local config commands ([#848](https://github.com/Gitlawb/zero/issues/848)) ([c52db05](https://github.com/Gitlawb/zero/commit/c52db055b0b213758a33c5c5627a411e052f8de4))
* **tui:** a copied NUL byte no longer panics the TUI on Windows ([#876](https://github.com/Gitlawb/zero/issues/876)) ([cae0269](https://github.com/Gitlawb/zero/commit/cae0269e77e7a0a493cf799b41c653c9ff9a19de)), closes [#875](https://github.com/Gitlawb/zero/issues/875)
* **tui:** exclude permission waits from turn time ([#820](https://github.com/Gitlawb/zero/issues/820)) ([15eb16f](https://github.com/Gitlawb/zero/commit/15eb16f736f903c94203207032f98aacbcf06318))
* **tui:** fall back to CellMotion where AllMotion is unreliable ([#872](https://github.com/Gitlawb/zero/issues/872)) ([7fbd864](https://github.com/Gitlawb/zero/commit/7fbd864e91433441489902571ea394ebc5b7bd0d))
* **tui:** fill alt-screen frame with theme panel background ([#850](https://github.com/Gitlawb/zero/issues/850)) ([020c28e](https://github.com/Gitlawb/zero/commit/020c28eaf95e2f38853f99fc06a13cd1568bf3c9))
* **tui:** hide unavailable sidebar shortcut ([#821](https://github.com/Gitlawb/zero/issues/821)) ([845df6c](https://github.com/Gitlawb/zero/commit/845df6c7d28e02f475e90acfe838733eb37e6aa2))
* use OAuth credentials in provider health checks ([#828](https://github.com/Gitlawb/zero/issues/828)) ([d37de92](https://github.com/Gitlawb/zero/commit/d37de9214b1ba86542f4f5ef3b060bb96849763b))


### Performance Improvements

* compact common test output ([#846](https://github.com/Gitlawb/zero/issues/846)) ([edf660a](https://github.com/Gitlawb/zero/commit/edf660ab85131f90a452cee51dc2c3a5cf62d632))
* improve harness context efficiency ([#838](https://github.com/Gitlawb/zero/issues/838)) ([8e26679](https://github.com/Gitlawb/zero/commit/8e266797a8305e689806eadcf9698da2796481ab))

## [0.6.0](https://github.com/Gitlawb/zero/compare/v0.5.0...v0.6.0) (2026-07-29)


### Features

* add persistent session goals ([#803](https://github.com/Gitlawb/zero/issues/803)) ([5d1869e](https://github.com/Gitlawb/zero/commit/5d1869e7bc193c97e07418b57b823905c1151d5d))
* **providers:** add Fireworks AI provider preset ([#772](https://github.com/Gitlawb/zero/issues/772)) ([58daea7](https://github.com/Gitlawb/zero/commit/58daea7a482927e5ab35afaf97761c60bb35e113))
* **skills:** report hash drift in skill info ([#795](https://github.com/Gitlawb/zero/issues/795)) ([9fbdc34](https://github.com/Gitlawb/zero/commit/9fbdc341551601c13434cf7b9b6f5e4ea4c483b1))


### Bug Fixes

* **acp:** interrupt idle reads on cancellation ([#782](https://github.com/Gitlawb/zero/issues/782)) ([15b7a54](https://github.com/Gitlawb/zero/commit/15b7a5475cfeefddbc59634551451a8872b996b7))
* **cli:** include plugins info in shell completions ([#794](https://github.com/Gitlawb/zero/issues/794)) ([fa8734f](https://github.com/Gitlawb/zero/commit/fa8734f17927de8832dfe041e043cfb86f93d12a))
* **config:** kill provider-command process tree via job object on Windows ([#690](https://github.com/Gitlawb/zero/issues/690)) ([5c58256](https://github.com/Gitlawb/zero/commit/5c58256abc29c185b449f8ce79647eb3fae61882))
* **config:** make the provider-command timeout an upper bound ([#811](https://github.com/Gitlawb/zero/issues/811)) ([#813](https://github.com/Gitlawb/zero/issues/813)) ([1c38e8c](https://github.com/Gitlawb/zero/commit/1c38e8cba50db255f3cda8b7cf4298712b2933e1))
* **config:** stop billing provider-command fixtures to the 5s timeout ([#810](https://github.com/Gitlawb/zero/issues/810)) ([f5fa4ff](https://github.com/Gitlawb/zero/commit/f5fa4ffc64537897aa78a56edca36d8f1dacd021))
* **cron:** reserve job IDs atomically ([#686](https://github.com/Gitlawb/zero/issues/686)) ([d9b882e](https://github.com/Gitlawb/zero/commit/d9b882eb0a94f38de8447cb54b57860ccd8ed053))
* **lint:** resolve static analysis baseline ([#769](https://github.com/Gitlawb/zero/issues/769)) ([583e653](https://github.com/Gitlawb/zero/commit/583e6530bd849a8efcb2b88bbe8c297e8592e62c))
* **providers:** retry provably pre-send transport failures ([#447](https://github.com/Gitlawb/zero/issues/447)) ([#750](https://github.com/Gitlawb/zero/issues/750)) ([8acd42a](https://github.com/Gitlawb/zero/commit/8acd42a3bd743a35807d762f23a33fb61c171839))
* **sandbox:** add platform-specific write lock around grant state file ([#752](https://github.com/Gitlawb/zero/issues/752)) ([#755](https://github.com/Gitlawb/zero/issues/755)) ([88fb7b6](https://github.com/Gitlawb/zero/commit/88fb7b6192c1ac8173089967d5e9119f92d7a101))
* **sandbox:** clarify scoped permission prompts ([#798](https://github.com/Gitlawb/zero/issues/798)) ([9fe02ae](https://github.com/Gitlawb/zero/commit/9fe02ae52e3234cb9dae76e0c5594a287ccbb52a))
* **sandbox:** deny reads of git's credential stores ([#815](https://github.com/Gitlawb/zero/issues/815)) ([#816](https://github.com/Gitlawb/zero/issues/816)) ([33c94ec](https://github.com/Gitlawb/zero/commit/33c94ec7a3e7987e6e36b5fd5e037ff4e6e84c26))
* **sandbox:** preserve user config and retry denied writes ([#801](https://github.com/Gitlawb/zero/issues/801)) ([81a5e6c](https://github.com/Gitlawb/zero/commit/81a5e6cb716ba4b7ecf7ff3ff261dfa51fd01098))
* **sandbox:** scrub the daemon token file pointer ([#677](https://github.com/Gitlawb/zero/issues/677)) ([#818](https://github.com/Gitlawb/zero/issues/818)) ([3e7692d](https://github.com/Gitlawb/zero/commit/3e7692d8f0885d346809869935c81a9cf055d89e))
* **swarm:** close lifecycle admission on shutdown ([#776](https://github.com/Gitlawb/zero/issues/776)) ([bdbda89](https://github.com/Gitlawb/zero/commit/bdbda89f3e42d93e8058ab3e1e9ef402659530cd))
* **tui:** fix rendering corruption over multipass + Windows Terminal ([#709](https://github.com/Gitlawb/zero/issues/709)) ([5bb1bc1](https://github.com/Gitlawb/zero/commit/5bb1bc11c3188dcea7709ff07c8b25dda3541de8))
* **tui:** paste clipboard images with Ctrl+V ([#534](https://github.com/Gitlawb/zero/issues/534)) ([#807](https://github.com/Gitlawb/zero/issues/807)) ([ec3b822](https://github.com/Gitlawb/zero/commit/ec3b8225050eb62f0f41a9582ed2881d2f9dcc8e))
* **tui:** reset quiet clock for new runs ([#799](https://github.com/Gitlawb/zero/issues/799)) ([a4a805a](https://github.com/Gitlawb/zero/commit/a4a805a6c82c51cb255e909bd87ca183c8805cf2))
* **windows:** use PowerShell for command execution ([#804](https://github.com/Gitlawb/zero/issues/804)) ([518d2e5](https://github.com/Gitlawb/zero/commit/518d2e53ba3fa073090a9b41c278face85b59b64))

## [0.5.0](https://github.com/Gitlawb/zero/compare/v0.4.0...v0.5.0) (2026-07-22)


### Features

* **plugins:** add zero plugins info command ([#773](https://github.com/Gitlawb/zero/issues/773)) ([2479884](https://github.com/Gitlawb/zero/commit/2479884dea49580c853a53536c3bbab22ce8a2bf))
* **sandbox:** disable the sandbox via config ([#687](https://github.com/Gitlawb/zero/issues/687)) ([#746](https://github.com/Gitlawb/zero/issues/746)) ([a21a052](https://github.com/Gitlawb/zero/commit/a21a052a4a32ce9cb3c93c94350cd24e138532cd))
* **sandbox:** unify command execution and enforcement ([#781](https://github.com/Gitlawb/zero/issues/781)) ([96859c9](https://github.com/Gitlawb/zero/commit/96859c9bd16f6dad4e332efc7e68178b9116118a))
* **tui:** add /undo as an alias for /rewind ([#698](https://github.com/Gitlawb/zero/issues/698)) ([#747](https://github.com/Gitlawb/zero/issues/747)) ([8c6d302](https://github.com/Gitlawb/zero/commit/8c6d3022bd7801eaad6a0440bb21a27221887297))
* **tui:** add isolated /btw conversations ([#748](https://github.com/Gitlawb/zero/issues/748)) ([2e267bd](https://github.com/Gitlawb/zero/commit/2e267bdbee4a77813e93a7ab51f0c575b66cee8c))
* **tui:** permission prompt takes free-text feedback inline ([#780](https://github.com/Gitlawb/zero/issues/780)) ([3967d49](https://github.com/Gitlawb/zero/commit/3967d49d64998341af8ef6d7523fc63aeb2a5a7a))


### Bug Fixes

* **agent:** stop write_stdin session_id probing thrash ([#702](https://github.com/Gitlawb/zero/issues/702)) ([#749](https://github.com/Gitlawb/zero/issues/749)) ([fbf8598](https://github.com/Gitlawb/zero/commit/fbf85984f679058125951fe2d5e4f200e09a3e2f))
* **cli:** warn when ZERO_PROVIDER overrides a providers-use selection ([#767](https://github.com/Gitlawb/zero/issues/767)) ([3524f79](https://github.com/Gitlawb/zero/commit/3524f795e5fdbb827166a84f03351cedfc9eba30))
* **doctor:** detect missing native binary during runtime checks ([#450](https://github.com/Gitlawb/zero/issues/450)) ([7796022](https://github.com/Gitlawb/zero/commit/77960229b839dca856616f846839aa773f2923f7))
* **lsp:** make the real-gopls check opt-in so a broken gopls can't fail the suite ([#684](https://github.com/Gitlawb/zero/issues/684)) ([#766](https://github.com/Gitlawb/zero/issues/766)) ([b1f4173](https://github.com/Gitlawb/zero/commit/b1f41735a7f6b7928a1875e986a4dca39d106cb9))
* make extension installs transactional ([#762](https://github.com/Gitlawb/zero/issues/762)) ([baa4be1](https://github.com/Gitlawb/zero/commit/baa4be13ac5321da4e9f53e864dd1cd395481200))
* **oauth:** refuse redirects on credential POSTs ([#729](https://github.com/Gitlawb/zero/issues/729)) ([#741](https://github.com/Gitlawb/zero/issues/741)) ([974fc03](https://github.com/Gitlawb/zero/commit/974fc036c2f9a194722f2e8fddbb4fdbf797effe))
* **oauth:** validate discovered endpoints before merge/use ([#511](https://github.com/Gitlawb/zero/issues/511)) ([#739](https://github.com/Gitlawb/zero/issues/739)) ([ce4a996](https://github.com/Gitlawb/zero/commit/ce4a996ffac4482e704f0fd61b3e442398fb2401))
* **perfbench:** absolutize the bench binary and make errored tasks first-class ([#730](https://github.com/Gitlawb/zero/issues/730)) ([dbd9443](https://github.com/Gitlawb/zero/commit/dbd94430143df6754d68551d1028ad8f15b82f1b))
* **perfbench:** grant write tools so mutating tasks measure real edits ([#763](https://github.com/Gitlawb/zero/issues/763)) ([e1975c1](https://github.com/Gitlawb/zero/commit/e1975c1b396b3236ed648870230b4f85863b1d03))
* **perfbench:** keep the stamped answer file out of negative oracle greps ([#737](https://github.com/Gitlawb/zero/issues/737)) ([015452c](https://github.com/Gitlawb/zero/commit/015452c1c98a39eabb021324182094e188a8bd47))
* **providers:** stop "provider not found" for env-derived profiles ([#716](https://github.com/Gitlawb/zero/issues/716)) ([4cbd144](https://github.com/Gitlawb/zero/commit/4cbd144d11e5cb67bc5fea46f5b294562dac7a1a))
* **sandbox:** AST second opinion for interactive-command bypasses ([#473](https://github.com/Gitlawb/zero/issues/473)) ([#745](https://github.com/Gitlawb/zero/issues/745)) ([f079b90](https://github.com/Gitlawb/zero/commit/f079b90f82dac7b7ae0864b279dc9094e31a6627))
* **sandbox:** bind Windows elevated ACL setup to one no-follow handle ([#765](https://github.com/Gitlawb/zero/issues/765)) ([4945684](https://github.com/Gitlawb/zero/commit/4945684fa26aa5994eda59dcabedc817423c535d))
* **sandbox:** don't auto-allow shell when re-entrancy skips wrapping ([#727](https://github.com/Gitlawb/zero/issues/727)) ([#744](https://github.com/Gitlawb/zero/issues/744)) ([6849011](https://github.com/Gitlawb/zero/commit/684901165d1b7f50a8bb4af31b1c1951e6926a79))
* **tools:** give write_stdin's invalid-session errors the same recovery guidance ([#749](https://github.com/Gitlawb/zero/issues/749) follow-up) ([#768](https://github.com/Gitlawb/zero/issues/768)) ([da9fb50](https://github.com/Gitlawb/zero/commit/da9fb50f549f6701eb136fc99f6178d0772d9334))
* **tools:** read_file recovers a backwards line range instead of erroring ([#779](https://github.com/Gitlawb/zero/issues/779)) ([89bdc67](https://github.com/Gitlawb/zero/commit/89bdc6719a1e1b3a3ef0e36b91a7839b3efdfba9))
* **tui:** cache settled alt-screen transcript ([#647](https://github.com/Gitlawb/zero/issues/647)) ([d74ceb1](https://github.com/Gitlawb/zero/commit/d74ceb11271ed68a21c19248210e098f411805fb))
* **tui:** model rows labelled by id when the description is prose; keep the sidebar under the / palette ([#775](https://github.com/Gitlawb/zero/issues/775)) ([b30c397](https://github.com/Gitlawb/zero/commit/b30c3971b40b06ba52ad649ba9dc0ad560d4be4c))
* **tui:** stop the permission card clashing on cool themes ([#778](https://github.com/Gitlawb/zero/issues/778)) ([722bb31](https://github.com/Gitlawb/zero/commit/722bb3121682d9cd9cd4bc6c127e9014d719262a))


### Performance Improvements

* **agent:** concurrent read-only tool batches via capability gate ([#715](https://github.com/Gitlawb/zero/issues/715)) ([31d45d5](https://github.com/Gitlawb/zero/commit/31d45d5f14e915acb9946e8b8eb81632c48f126a))
* **agent:** execution profiles with one-shot posture escalation (PR10b+PR10c) ([#740](https://github.com/Gitlawb/zero/issues/740)) ([378d538](https://github.com/Gitlawb/zero/commit/378d538e240c289e57821e9f9628f76034419d01))
* **agent:** posture-escalation signals and controller (PR10a) ([#736](https://github.com/Gitlawb/zero/issues/736)) ([af875df](https://github.com/Gitlawb/zero/commit/af875df58775484304fc65586bd6c74552ad01a2))
* **agent:** preserve prompt cache prefixes ([#760](https://github.com/Gitlawb/zero/issues/760)) ([739a47e](https://github.com/Gitlawb/zero/commit/739a47e3eac92c3decc8734f52a4d99c7480c3ca))
* **openai:** optimized turn session — background prewarm and prefix telemetry (PR8) ([#723](https://github.com/Gitlawb/zero/issues/723)) ([60dc84e](https://github.com/Gitlawb/zero/commit/60dc84e7a38c5544ebc047f3cfaf4625dd1e83b5))
* **output:** add token-aware semantic output budgeting (PR11) ([#717](https://github.com/Gitlawb/zero/issues/717)) ([e5670c4](https://github.com/Gitlawb/zero/commit/e5670c427ff39c628fb0822fd5c9317ee2174583))
* **providers:** provider capabilities and default turn-session adapter (PR7) ([#720](https://github.com/Gitlawb/zero/issues/720)) ([30e2c3f](https://github.com/Gitlawb/zero/commit/30e2c3f7ffa1d5e487bd10b59d4e823cda191d48))
* **turn-bench:** Phase 0 — strengthen oracles so pass rate can't be misread as correctness ([#712](https://github.com/Gitlawb/zero/issues/712)) ([727ad4d](https://github.com/Gitlawb/zero/commit/727ad4d321fab45d0cf40f8535522e3d94e55c4a))

## [0.4.0](https://github.com/Gitlawb/zero/compare/v0.3.0...v0.4.0) (2026-07-17)


### Features

* **aimlapi:** AI/ML API provider with guided onboarding (top-up + key issuance) ([#655](https://github.com/Gitlawb/zero/issues/655)) ([6b9c2f0](https://github.com/Gitlawb/zero/commit/6b9c2f0c083e02bcd3aad68fb7af0501a9e7cd61))
* **cli:** show ZERO wordmark on --version ([#673](https://github.com/Gitlawb/zero/issues/673)) ([9acb411](https://github.com/Gitlawb/zero/commit/9acb4113cc3337b3f361a16278e9cf11ca105e34))
* **cli:** wire MCP serve WorkspaceRoot and --add-dir scope ([#694](https://github.com/Gitlawb/zero/issues/694)) ([75a78e7](https://github.com/Gitlawb/zero/commit/75a78e715bf23154f69c8c79cb58eb4b535b2a2a))
* **npm:** ship the native binary as platform optionalDependencies ([#626](https://github.com/Gitlawb/zero/issues/626)) ([5e1405d](https://github.com/Gitlawb/zero/commit/5e1405d0b7abff5b3ccb3cfdb66d64d6d3322922))
* **perf:** emit prompt-prefix hash fingerprint per turn ([#704](https://github.com/Gitlawb/zero/issues/704)) ([1c5c6e7](https://github.com/Gitlawb/zero/commit/1c5c6e78a8a0e228bdf53d7be90934fbce9d98c3))
* **providers:** add AI/ML API preset (rebased onto main) ([#621](https://github.com/Gitlawb/zero/issues/621)) ([d66a9dd](https://github.com/Gitlawb/zero/commit/d66a9dda69c32aa59f4bea903cfefe00d4b7adef))
* **providers:** refresh MiniMax model coverage ([#665](https://github.com/Gitlawb/zero/issues/665)) ([fa3052a](https://github.com/Gitlawb/zero/commit/fa3052a1422a4ad30a3a6295829564f42dee31a8))
* **skills:** discover shared ~/.agents/skills with multi-root skill loading ([#696](https://github.com/Gitlawb/zero/issues/696)) ([7d57999](https://github.com/Gitlawb/zero/commit/7d579996b43741a2e57e3e38fcdd8484fcfc34e9))
* **tui:** Ctrl+X leader chords and emacs menu navigation ([#699](https://github.com/Gitlawb/zero/issues/699)) ([7f669f4](https://github.com/Gitlawb/zero/commit/7f669f455021be51319ee3b8298cd48a17f745c7))
* **tui:** press up to edit queued messages ([#656](https://github.com/Gitlawb/zero/issues/656)) ([4c986d3](https://github.com/Gitlawb/zero/commit/4c986d327b5ed26338d295c23bea5839681db8d0))


### Bug Fixes

* **acp:** make truncateHint rune-safe ([#614](https://github.com/Gitlawb/zero/issues/614)) ([ddc4927](https://github.com/Gitlawb/zero/commit/ddc4927aac5544bf4dd2c46615aeda5a81c96576))
* **agent,tui:** resolve git branch detection when starting Zero in subdirectories ([#613](https://github.com/Gitlawb/zero/issues/613)) ([0184581](https://github.com/Gitlawb/zero/commit/0184581ec234ea414e0a47bc33e8b0f4ddfb497b))
* **agent:** raise default and deep-mode turn budgets ([#650](https://github.com/Gitlawb/zero/issues/650)) ([635c93a](https://github.com/Gitlawb/zero/commit/635c93af51ebc20f3e0917917e55a79edfe27c35))
* **cli:** prevent consuming positional arguments as flag values ([#619](https://github.com/Gitlawb/zero/issues/619)) ([5b4f48d](https://github.com/Gitlawb/zero/commit/5b4f48d2dcb66402c13bde0c3cfe9c9371da19fb))
* **config:** enforce MCP trust boundary so project config cannot override user disable ([#609](https://github.com/Gitlawb/zero/issues/609)) ([4d8c31c](https://github.com/Gitlawb/zero/commit/4d8c31cf16a3080344e3be7039408fcae71c075d)), closes [#512](https://github.com/Gitlawb/zero/issues/512)
* **config:** surface unknown/typo'd config fields instead of silently dropping them ([#645](https://github.com/Gitlawb/zero/issues/645)) ([893b7b4](https://github.com/Gitlawb/zero/commit/893b7b424cc203a2fcf92327a4e25c84286a90e0))
* **cron:** prevent cron job Mutate from clobbering concurrent updates ([#630](https://github.com/Gitlawb/zero/issues/630)) ([e4bd703](https://github.com/Gitlawb/zero/commit/e4bd703cfb28dab2dfa3c2ddba46237e1bb2e164))
* **daemon:** handle os.ErrPermission as collision during O_EXCL lock creation ([#616](https://github.com/Gitlawb/zero/issues/616)) ([8ea5384](https://github.com/Gitlawb/zero/commit/8ea53841a5d54c37a153779ae86bea010659433c))
* **exec:** stop false INCOMPLETE downgrades on conversational final messages ([#608](https://github.com/Gitlawb/zero/issues/608)) ([b6117af](https://github.com/Gitlawb/zero/commit/b6117af86d6bc87a4ee66910e99d76cb16b03fed))
* harden MCP credential boundaries ([#597](https://github.com/Gitlawb/zero/issues/597)) ([fdddb05](https://github.com/Gitlawb/zero/commit/fdddb05ba84b1600ae6c3a20028bf83afe474c44))
* **hooks:** fail closed on launch failures for beforeTool hooks ([#629](https://github.com/Gitlawb/zero/issues/629)) ([dc06fe7](https://github.com/Gitlawb/zero/commit/dc06fe72caf45f72d2cba1e8a835c0f5b405c1e8))
* **hooks:** run sessionEnd hooks after Esc/Ctrl+C interrupts ([#606](https://github.com/Gitlawb/zero/issues/606)) ([824ecdb](https://github.com/Gitlawb/zero/commit/824ecdbcf9c467c35ef4e2666770fdadcb5bf402))
* **keyring:** pass generic password via stdin on macOS ([#574](https://github.com/Gitlawb/zero/issues/574)) ([91ea6de](https://github.com/Gitlawb/zero/commit/91ea6ded7503538834a84d090f78670a363c62d3))
* **lock:** prevent POSIX lock file overwrite and leak on Windows/Unix ([#628](https://github.com/Gitlawb/zero/issues/628)) ([da41c3a](https://github.com/Gitlawb/zero/commit/da41c3a75b782d6e0836fe13346321e40a90fbb4))
* **openai:** omit prompt_cache_key for openai-compatible providers ([#636](https://github.com/Gitlawb/zero/issues/636)) ([1af5882](https://github.com/Gitlawb/zero/commit/1af58828eb3c22567599c000736c913a290959d2)), closes [#624](https://github.com/Gitlawb/zero/issues/624)
* **plugins:** resolve relative executable paths against plugin root ([#627](https://github.com/Gitlawb/zero/issues/627)) ([2efe6d5](https://github.com/Gitlawb/zero/commit/2efe6d539e29374b3ef39c2290bdea81f33a228b))
* **sandbox:** remove windowsWriteRestricted flag to fix DenyRead bypass ([#612](https://github.com/Gitlawb/zero/issues/612)) ([3d96ac7](https://github.com/Gitlawb/zero/commit/3d96ac7e55c760a97f28c0e6ceaf1ec3b4ab717a))
* **sandbox:** scrub dynamic credential env vars ([#682](https://github.com/Gitlawb/zero/issues/682)) ([9043bae](https://github.com/Gitlawb/zero/commit/9043baedcff7776c7373645b57563cac06b31847))
* **sandbox:** scrub sensitive credentials from sandbox environment ([#660](https://github.com/Gitlawb/zero/issues/660)) ([6fc1220](https://github.com/Gitlawb/zero/commit/6fc1220f6ac66fb3ae67b637cbbed7068d2213c0))
* **sandbox:** unblock git fetch/commit/add under the write-restricted sandbox ([#654](https://github.com/Gitlawb/zero/issues/654)) ([5c4815a](https://github.com/Gitlawb/zero/commit/5c4815a66ed07d9cf90b825adfd936d3ac07639d))
* **sandbox:** use WRITE_RESTRICTED token when no DenyRead paths are configured ([#658](https://github.com/Gitlawb/zero/issues/658)) ([a5d2e32](https://github.com/Gitlawb/zero/commit/a5d2e327c8681671aa8a9e5378801215b747edcf))
* **securefile,credstore:** call Sync on temp file before close and rename ([#631](https://github.com/Gitlawb/zero/issues/631)) ([212734a](https://github.com/Gitlawb/zero/commit/212734adf3b5f982e22385161205aa1afe4634fe))
* **securefile:** reclaim stale lock files to prevent permanent DOS ([#615](https://github.com/Gitlawb/zero/issues/615)) ([8536cc8](https://github.com/Gitlawb/zero/commit/8536cc87f7a885f9e436d6ef28f5f325201623dc))
* **swarm:** wait for job.Runs directly in scheduler skip test ([#667](https://github.com/Gitlawb/zero/issues/667)) ([1bb6b57](https://github.com/Gitlawb/zero/commit/1bb6b5745af90321d1e657a12a1976cded5dd1bd))
* **tools:** classify silent wrapped Windows command failures as sandbox denials ([#659](https://github.com/Gitlawb/zero/issues/659)) ([8bd9742](https://github.com/Gitlawb/zero/commit/8bd9742fa95c41b93ea4e718628aed0ff3ae9dd0))
* **tools:** preserve SysProcAttr during PTY fallback ([#618](https://github.com/Gitlawb/zero/issues/618)) ([f78b36c](https://github.com/Gitlawb/zero/commit/f78b36c770daa4577a9f99265b18a354454e36eb))
* **tui:** resolve pending askUser callbacks to prevent runner hangs ([#620](https://github.com/Gitlawb/zero/issues/620)) ([aa73a76](https://github.com/Gitlawb/zero/commit/aa73a76f1bd1b6fe97bac2fbff2d61b7474139f2))
* **tui:** stop the composer cursor blinking while typing or unfocused ([#672](https://github.com/Gitlawb/zero/issues/672)) ([2b42cd5](https://github.com/Gitlawb/zero/commit/2b42cd567b96d6f7e0818594e53ff31cce1e42e9))
* **windows:** resolve absolute path for taskkill to prevent hijacking ([#617](https://github.com/Gitlawb/zero/issues/617)) ([2db00ee](https://github.com/Gitlawb/zero/commit/2db00ee3d57db97e0fbe23cb8628e8bcb47f6f09))


### Performance Improvements

* **tools:** add explicit effect metadata for safe concurrency ([#705](https://github.com/Gitlawb/zero/issues/705)) ([8ef8576](https://github.com/Gitlawb/zero/commit/8ef8576df7d0775a5b815bc0776a794cadb75c34))

## [0.3.0](https://github.com/Gitlawb/zero/compare/v0.2.0...v0.3.0) (2026-07-09)


### Features

* gate project-scoped hooks, plugins, and MCP servers behind workspace trust ([#529](https://github.com/Gitlawb/zero/issues/529)) ([a880ce8](https://github.com/Gitlawb/zero/commit/a880ce80a6ec72da511fb9bdf6dd69291c72a64b))
* **modelregistry:** infer reasoning efforts for Hunyuan and vendor-prefixed model ids ([#599](https://github.com/Gitlawb/zero/issues/599)) ([92a92ce](https://github.com/Gitlawb/zero/commit/92a92ceb29dc63f5216ab140ef9a6dd9afe17df8))
* **providers:** OAuth login profiles and list-first /provider manager ([#560](https://github.com/Gitlawb/zero/issues/560)) ([1655056](https://github.com/Gitlawb/zero/commit/16550569c1be615cfaf244dab25909ec37f6dee6))
* **tui:** remember recent provider+model selections in /model picker ([#568](https://github.com/Gitlawb/zero/issues/568)) ([d0c4e62](https://github.com/Gitlawb/zero/commit/d0c4e62cd429a0614c15296934c740a08bc0e07b))
* **tui:** show CLI version on the startup home screen ([#538](https://github.com/Gitlawb/zero/issues/538)) ([fd69233](https://github.com/Gitlawb/zero/commit/fd69233e334f1823a06b5794085a9255b3abdfa8))
* voice dictation (speech-to-text) ([#557](https://github.com/Gitlawb/zero/issues/557)) ([87158a1](https://github.com/Gitlawb/zero/commit/87158a1c90b4f91fc5f2bb8178ebaf46d7654680))


### Bug Fixes

* address bugs found in a multi-agent codebase audit ([#481](https://github.com/Gitlawb/zero/issues/481)) ([008bc9b](https://github.com/Gitlawb/zero/commit/008bc9b3f3ba13c7d4822b9559b020f381ff555b))
* **agent:** keep tools exposed for max-turn finalization ([#533](https://github.com/Gitlawb/zero/issues/533)) ([3f0503b](https://github.com/Gitlawb/zero/commit/3f0503bc2312ae29d5ade784d8824dc9a3524958))
* **auth:** persist OpenRouter API key after login ([#595](https://github.com/Gitlawb/zero/issues/595)) ([2a062aa](https://github.com/Gitlawb/zero/commit/2a062aa4014c6cd5e20a57dad4a685f88966f109))
* bump Go to 1.26.5 for crypto/tls fix (GO-2026-5856) ([#607](https://github.com/Gitlawb/zero/issues/607)) ([a7cfb99](https://github.com/Gitlawb/zero/commit/a7cfb99fed7b88ebc09a2f251cb82864d3c2cade))
* gitignore Windows sandbox helpers and npm version marker ([#578](https://github.com/Gitlawb/zero/issues/578)) ([25653f6](https://github.com/Gitlawb/zero/commit/25653f686c016f95b81ceb7ff5d5452d37c4d4f3))
* **mcp:** silence startup warning for unconfigured default servers ([#563](https://github.com/Gitlawb/zero/issues/563)) ([302f58b](https://github.com/Gitlawb/zero/commit/302f58bb5f2a03ec7230354ed4747e4e55c16c50))
* **mcp:** skip RFC 8414 discovery when OAuth endpoints are preconfigured ([#586](https://github.com/Gitlawb/zero/issues/586)) ([8a52d98](https://github.com/Gitlawb/zero/commit/8a52d98cad7cd0086dee9aede4ce477e432bd385))
* **modelregistry:** reject oversized models.dev cache responses ([#602](https://github.com/Gitlawb/zero/issues/602)) ([66a6396](https://github.com/Gitlawb/zero/commit/66a63964149fb2f07e646e5f1987627c5cd9ac28))
* **provider:** stop dropping custom no-auth providers on restart ([#558](https://github.com/Gitlawb/zero/issues/558)) ([ba99fa8](https://github.com/Gitlawb/zero/commit/ba99fa8d487fb28f2700e0ff10b2a25c75303cf7))
* **sandbox:** Windows-appropriate suggestions when blocking interactive commands ([#414](https://github.com/Gitlawb/zero/issues/414)) ([ba4c007](https://github.com/Gitlawb/zero/commit/ba4c00755dc1c31b3dcca18e50b20f62c6bf5d1f))
* **tools:** block MSYS and WSL shells under the Windows sandbox ([#587](https://github.com/Gitlawb/zero/issues/587)) ([0666818](https://github.com/Gitlawb/zero/commit/066681855b80e3baf5d07d7397610b25f724e353))
* **tools:** platform-specific pager suggestions, quote/caret-safe cd detection ([#543](https://github.com/Gitlawb/zero/issues/543)) ([8b248f4](https://github.com/Gitlawb/zero/commit/8b248f4e1198dc86ab332a697fba4cf520823cbd))
* **tools:** Windows cmd.exe quoting guidance and clipboard escaping fix ([#468](https://github.com/Gitlawb/zero/issues/468)) ([f10ed0c](https://github.com/Gitlawb/zero/commit/f10ed0c893ce6de08923f143d681ba96f0fcfe3a))
* **tui:** bypass toggleSidebar and toggleMouse global shortcuts when composer is non-empty ([#576](https://github.com/Gitlawb/zero/issues/576)) ([c7346fb](https://github.com/Gitlawb/zero/commit/c7346fbcf01fe7e70ed2fcfccbf9965e5985727b))
* **tui:** paste protection for Termux char-by-char paste ([#573](https://github.com/Gitlawb/zero/issues/573)) ([8e9149f](https://github.com/Gitlawb/zero/commit/8e9149f1a296bebebf95cae9dd2a7c5156c9dbb6))
* **tui:** update picker_test for switchProviderModel 4-value return ([#589](https://github.com/Gitlawb/zero/issues/589)) ([8f15650](https://github.com/Gitlawb/zero/commit/8f156506dc92449bd24caa14e36f28477ba00fff))
* **update:** clearer error on unsupported release platform (android/termux) ([#603](https://github.com/Gitlawb/zero/issues/603)) ([1fc9b2d](https://github.com/Gitlawb/zero/commit/1fc9b2d25c79e089f34cb7b5b6a7f7c7b8233123))
* **update:** support safe symlink extraction during updates ([#575](https://github.com/Gitlawb/zero/issues/575)) ([ce9cb91](https://github.com/Gitlawb/zero/commit/ce9cb912958a3d9ae0b052bfebb849c28e0e719b))
* warn about untracked scratch files left behind after a run ([#571](https://github.com/Gitlawb/zero/issues/571)) ([062328b](https://github.com/Gitlawb/zero/commit/062328b632a2d27353a6d47c522bbd22d7282539))


### Performance Improvements

* **grep:** stop content scan after head limit ([#601](https://github.com/Gitlawb/zero/issues/601)) ([8a05e64](https://github.com/Gitlawb/zero/commit/8a05e6486f7a63281b869064db03dfc5531e6a04))

## [0.2.0](https://github.com/Gitlawb/zero/compare/v0.1.0...v0.2.0) (2026-07-06)


### Features

* add --auto flag for LLM-generated commit messages ([#423](https://github.com/Gitlawb/zero/issues/423)) ([b0abde7](https://github.com/Gitlawb/zero/commit/b0abde7d0697e808480cd59d69a6f4d0c6320475))
* add zero changes push and pr subcommands, and extra repo-info metrics ([#391](https://github.com/Gitlawb/zero/issues/391)) ([2312abe](https://github.com/Gitlawb/zero/commit/2312abe5ddd95f4c6ef373cfb61cc03092f48cdd))
* agent quality, caching, retry, and tooling upgrades ([#506](https://github.com/Gitlawb/zero/issues/506)) ([3c81fea](https://github.com/Gitlawb/zero/commit/3c81fea22873ee3df7fc97b10cb4f77792706c4b))
* **agent:** curb over-engineering the solution in the editing discipline ([#517](https://github.com/Gitlawb/zero/issues/517)) ([f4c998a](https://github.com/Gitlawb/zero/commit/f4c998ac30c4f07ff313a2d706791e857293be49)), closes [#516](https://github.com/Gitlawb/zero/issues/516)
* **agent:** inject per-user config.UserConfigDir()/zero/ZERO.md guidelines into system prompt ([#475](https://github.com/Gitlawb/zero/issues/475)) ([7b10aab](https://github.com/Gitlawb/zero/commit/7b10aab74bf14a01166b2cea22deab79bba9850b))
* **openai:** forward prompt_cache_key for server-side prefix cache routing ([#515](https://github.com/Gitlawb/zero/issues/515)) ([87e7e69](https://github.com/Gitlawb/zero/commit/87e7e69afd18b5539579856f3a61c6a95bc445ae))
* **providers:** add `zero providers models` to discover a provider's models ([#386](https://github.com/Gitlawb/zero/issues/386)) ([0bc8074](https://github.com/Gitlawb/zero/commit/0bc8074c97b0310e4a9d70c3f967003ee5e8a59f))
* **providers:** add KiloCode and OpenCode provider support ([#388](https://github.com/Gitlawb/zero/issues/388)) ([b1ccb6d](https://github.com/Gitlawb/zero/commit/b1ccb6d9c1875377f5e5ea81a1304edd1e41ab4f))
* **providers:** add Meituan LongCat catalog preset ([#424](https://github.com/Gitlawb/zero/issues/424)) ([b4275e3](https://github.com/Gitlawb/zero/commit/b4275e350472b2490212bf814709819d354c1216))
* **providers:** split minimax zai into intl cn ([#398](https://github.com/Gitlawb/zero/issues/398)) ([aaad4d2](https://github.com/Gitlawb/zero/commit/aaad4d271270f41af837b6f3b60ae80beba0c645))
* require manual approval before npm publish + drop release-as pin ([#369](https://github.com/Gitlawb/zero/issues/369)) ([bd89a1f](https://github.com/Gitlawb/zero/commit/bd89a1f451643c1b65ec803070abc7b116631ebe))
* **sandbox:** unelevated Windows fallback tier instead of prompts-only degrade ([#427](https://github.com/Gitlawb/zero/issues/427)) ([b9ddd6f](https://github.com/Gitlawb/zero/commit/b9ddd6f42138312a1fee8d8bb67c46c8eb1dea2f))
* support shift enter for composer newlines ([#462](https://github.com/Gitlawb/zero/issues/462)) ([daf65e0](https://github.com/Gitlawb/zero/commit/daf65e0af9a040314d4ab337b0ad59c55416b7bc))
* **tui:** /loop — repeat a prompt or command on an interval or self-paced ([#502](https://github.com/Gitlawb/zero/issues/502)) ([387fe67](https://github.com/Gitlawb/zero/commit/387fe67ee7cd81317c9c969f5906a4437080fea3))
* **tui:** add search/filter to provider picker in setup wizard ([#400](https://github.com/Gitlawb/zero/issues/400)) ([2fcea71](https://github.com/Gitlawb/zero/commit/2fcea71778d23e050c93409c471aef45b68c1621))
* **update:** add zero upgrade command to apply self-updates ([#461](https://github.com/Gitlawb/zero/issues/461)) ([5f36349](https://github.com/Gitlawb/zero/commit/5f36349c1884e81fa9bc66bb5fe813b627e897b7))


### Bug Fixes

* **action:** keep provider key scoped to zero step ([#448](https://github.com/Gitlawb/zero/issues/448)) ([407a927](https://github.com/Gitlawb/zero/commit/407a92739ff508cba32d2c12b3f36f0efcdd54c3))
* add android platform support for Termux npm install ([#455](https://github.com/Gitlawb/zero/issues/455)) ([9bd93c6](https://github.com/Gitlawb/zero/commit/9bd93c62f8d57fb74057284aa66a1b6e1429dcdd)), closes [#449](https://github.com/Gitlawb/zero/issues/449)
* **agent:** reject a malformed additional_permissions payload before prompting ([#453](https://github.com/Gitlawb/zero/issues/453)) ([e4f760e](https://github.com/Gitlawb/zero/commit/e4f760ee8bd57299cd2fcb37e8e23130037c2607))
* allow non-TLS connections to private-network provider endpoints ([#444](https://github.com/Gitlawb/zero/issues/444)) ([1d86384](https://github.com/Gitlawb/zero/commit/1d8638466ca31517eb9db2b9353d3dce1cbeeabc))
* **auth:** route zero auth login chatgpt to the dedicated ChatGPT flow ([#443](https://github.com/Gitlawb/zero/issues/443)) ([305a62c](https://github.com/Gitlawb/zero/commit/305a62c954ca6cec00bc58d5398f933415156aff))
* **config:** fall back to a usable saved provider instead of forcing full re-onboarding ([#410](https://github.com/Gitlawb/zero/issues/410)) ([c60ad87](https://github.com/Gitlawb/zero/commit/c60ad8729f79bb841114d352ee2d2fe29d5d0e41))
* **config:** let a gateway ANTHROPIC_BASE_URL resolve as anthropic-compatible ([#497](https://github.com/Gitlawb/zero/issues/497)) ([30dd7c3](https://github.com/Gitlawb/zero/commit/30dd7c3112ad22d42fa12b5addd4e38f4beda42a)), closes [#479](https://github.com/Gitlawb/zero/issues/479)
* **config:** unbrick first-run setup — default google/anthropic models, enter setup on fixable config errors ([#385](https://github.com/Gitlawb/zero/issues/385)) ([72eed06](https://github.com/Gitlawb/zero/commit/72eed06b4f94c43d75d31fe54a58d2f566de059e))
* **config:** use ~/.config on macOS and enter setup when no provider ([#371](https://github.com/Gitlawb/zero/issues/371)) ([#372](https://github.com/Gitlawb/zero/issues/372)) ([027a8f2](https://github.com/Gitlawb/zero/commit/027a8f2768b17b89f5c8270887f156e2ccda69ea))
* **docs:** rename AGENTS.MD &gt; AGENTS.md ([#438](https://github.com/Gitlawb/zero/issues/438)) ([4266baf](https://github.com/Gitlawb/zero/commit/4266baf222df583ed2078b776687f12d496475b5))
* **gemini:** strip unsupported JSON Schema fields from tool declarations ([#374](https://github.com/Gitlawb/zero/issues/374)) ([39e7100](https://github.com/Gitlawb/zero/commit/39e7100674150144a1152e3110c64c7cf0321d64)), closes [#373](https://github.com/Gitlawb/zero/issues/373)
* **install:** persist install dir to user PATH on Windows ([#407](https://github.com/Gitlawb/zero/issues/407)) ([bdb1b0e](https://github.com/Gitlawb/zero/commit/bdb1b0ecd15859b1712a6037d296dace7f9c3c3f))
* **mcp:** block cross-origin credential redirects ([#396](https://github.com/Gitlawb/zero/issues/396)) ([f915f70](https://github.com/Gitlawb/zero/commit/f915f70e5a3096e2419fa8d961a0f84a626fa4a9))
* **oauth:** treat Windows ERROR_ACCESS_DENIED as lock contention in createSecretFile ([#445](https://github.com/Gitlawb/zero/issues/445)) ([d05e914](https://github.com/Gitlawb/zero/commit/d05e9148a7f79f67d1d3c31fca2775f21fbd331e))
* **openai:** handle Ollama reasoning stream deltas ([#486](https://github.com/Gitlawb/zero/issues/486)) ([f6c0606](https://github.com/Gitlawb/zero/commit/f6c060631e18e082dda24cc4dc0903c31c2120d6))
* preserve conversation context in exec prompts ([#460](https://github.com/Gitlawb/zero/issues/460)) ([949ee43](https://github.com/Gitlawb/zero/commit/949ee43f71e5cb7fab4695c5cb7b442fe4ecfbf7))
* **provider-wizard:** allow multiple custom OpenAI-compatible providers ([#403](https://github.com/Gitlawb/zero/issues/403)) ([3fbbd28](https://github.com/Gitlawb/zero/commit/3fbbd28e4c586822cc4312c86232d94befe56e87))
* **sandbox:** fix nested pipe creation under the Windows restricted token ([#456](https://github.com/Gitlawb/zero/issues/456)) ([563a6db](https://github.com/Gitlawb/zero/commit/563a6dbe91e65d5daeefd7626e8a77e30a6d8fb2))
* **sandbox:** gate /tmp test assertions on GOOS, not path existence ([#426](https://github.com/Gitlawb/zero/issues/426)) ([f653dca](https://github.com/Gitlawb/zero/commit/f653dcac363fb69ad7be5b35e6e0fa6d2bce476d))
* **sandbox:** self-heal a corrupt unelevated setup marker ([#437](https://github.com/Gitlawb/zero/issues/437)) ([8d0c5fe](https://github.com/Gitlawb/zero/commit/8d0c5feccb8bdbfb015df0508aa6e3bcbd1fd0e8))
* **specialist:** cap max specialist nesting depth ([#491](https://github.com/Gitlawb/zero/issues/491)) ([177442c](https://github.com/Gitlawb/zero/commit/177442cfe4015bd8df04cc9894f98b468ee796d4))
* Termux/Android support — PRoot scroll, SIGSYS sandbox, build docs ([#509](https://github.com/Gitlawb/zero/issues/509)) ([0f69d99](https://github.com/Gitlawb/zero/commit/0f69d995e9b586b774f66c066b21abab5e03024a))
* **tools:** Block MSYS coreutils under Windows sandbox ([#476](https://github.com/Gitlawb/zero/issues/476)) ([81aad58](https://github.com/Gitlawb/zero/commit/81aad58d97839e51c068e2f08907618991fdc3fb))
* **tools:** CRLF line ending mismatch in edit_file tool on Windows ([#378](https://github.com/Gitlawb/zero/issues/378)) ([33dc7ae](https://github.com/Gitlawb/zero/commit/33dc7ae2cc82c5389675531e1416856dae7151ce))
* **tools:** fix cmd.exe /S/C corrupting commands with embedded quotes ([#465](https://github.com/Gitlawb/zero/issues/465)) ([190241b](https://github.com/Gitlawb/zero/commit/190241bd593f43211b766e0b13c8e89802d4bb37))
* **tools:** flag piped POSIX utilities before running on Windows ([#412](https://github.com/Gitlawb/zero/issues/412)) ([5658a36](https://github.com/Gitlawb/zero/commit/5658a366274fc59a9d5336b06a21019c9c25cbf1))
* **tools:** make grep and glob respect run cancellation ([#464](https://github.com/Gitlawb/zero/issues/464)) ([ba6c026](https://github.com/Gitlawb/zero/commit/ba6c0264697b7d7ed479f6e782fba9700a481e3d))
* **tools:** require permission before web_search requests ([#382](https://github.com/Gitlawb/zero/issues/382)) ([960db96](https://github.com/Gitlawb/zero/commit/960db9660e4e31dc588fe8f7d6f116ff5e225566))
* **tui:** compose help overlay through the viewport overlay pipeline ([#421](https://github.com/Gitlawb/zero/issues/421)) ([5b2b4de](https://github.com/Gitlawb/zero/commit/5b2b4dea1aaf9e0f68baa25e97e83296fb17b1a2))
* **tui:** keep the profile name on /model switch so the stored key resolves ([#441](https://github.com/Gitlawb/zero/issues/441)) ([9134148](https://github.com/Gitlawb/zero/commit/9134148f4df3e4e556fba6c2f8babfdf6fcfeee1)), closes [#440](https://github.com/Gitlawb/zero/issues/440)
* **tui:** resolve every permission request so the agent can't deadlock ([#397](https://github.com/Gitlawb/zero/issues/397)) ([952788f](https://github.com/Gitlawb/zero/commit/952788f72d32957659fe004521fcc8372b9ba9b4))
* **tui:** show an M suffix for million-scale token counts ([#457](https://github.com/Gitlawb/zero/issues/457)) ([0562e3b](https://github.com/Gitlawb/zero/commit/0562e3bef7df2328610a48a1e81632a8da4aec64))
* **tui:** title /model rows by model name, not the catalog description ([#395](https://github.com/Gitlawb/zero/issues/395)) ([cdf9d83](https://github.com/Gitlawb/zero/commit/cdf9d839ae57a729f292f36f7c5b0c67b41b288d))


### Performance Improvements

* cache TUI model registry ([#496](https://github.com/Gitlawb/zero/issues/496)) ([e7d88b4](https://github.com/Gitlawb/zero/commit/e7d88b4b518049733da25a8447c00144bd1da518))
* universal tool-output ceiling with spill + async post-edit diagnostics ([#518](https://github.com/Gitlawb/zero/issues/518)) ([95ccd5b](https://github.com/Gitlawb/zero/commit/95ccd5bc327f6fb464ff0239f7229de789f578dc))

## 0.1.0 (2026-07-02)


### Features

* publish zero to npm via release-please ([#367](https://github.com/Gitlawb/zero/issues/367)) ([8eccc26](https://github.com/Gitlawb/zero/commit/8eccc2669887bc38d35bc16a315c888e4d9ec43a))
* **tui:** FILES sidebar panel with click-to-select and file drill-in ([#365](https://github.com/Gitlawb/zero/issues/365)) ([142c548](https://github.com/Gitlawb/zero/commit/142c548c89a8652ce300e64ddf1228ee36df7606))


### Bug Fixes

* **auth:** propagate credentials to every provider-build surface and pin children to the live provider ([#366](https://github.com/Gitlawb/zero/issues/366)) ([6e0a665](https://github.com/Gitlawb/zero/commit/6e0a665118fe0e09c4b07d482dd18f86045acd2b))

## [Unreleased]

### Added
- Shared multi-agent skills discovery: when present, `~/.agents/skills` is searched after the primary
  Zero skills dir (and before plugin skill roots). `zero skills list` / `info` and the runtime `skill`
  tool share one multi-root discovery path; install/remove/lock still target only the Zero skills directory.
- `SECURITY.md` with a private vulnerability-reporting path, `CODE_OF_CONDUCT.md`, this changelog, and
  GitHub issue/PR templates.
- Interactive `/theme` picker: bare `/theme` opens a popup that live-previews each palette as you move
  and applies on select (Esc reverts).
- Twelve built-in color themes alongside the `dark`/`light` built-ins — `dracula`, `nord`, `gruvbox`,
  `tokyo-night`, `catppuccin`, `one-dark`, `solarized-dark`, `rose-pine`, `everforest`,
  `solarized-light`, `dune`, and `neon` — selectable via `/theme <name>`, `--theme <name>`, or
  `ZERO_THEME`. Every palette is contrast-audited to WCAG AA, and the new presets are additionally
  audited after xterm-256 downsampling; see [docs/THEMES.md](docs/THEMES.md). The built-in light
  theme was reworked for legibility.
- `--theme <name>` flag for the TUI, accepting `auto` or any registered theme (previously only the
  `ZERO_THEME` env var existed).
- "Accessibility / Appearance" section in the README documenting `NO_COLOR`, `ZERO_THEME`, `/theme`,
  and `ZERO_NO_FADE`.

### Changed
- Provider connectivity health checks now allow loopback hosts for explicitly user-configured local
  providers (Ollama / LM Studio), so the keyless local-model path verifies instead of failing with
  "localhost hosts are blocked". The SSRF guard for fetched/remote URLs is unchanged.
- Auth (401/403) errors now show a curated, actionable message pointing at `zero auth` / setup; the
  raw upstream body is shown only under a verbose/debug flag.
- No-provider / missing-key errors now point at `zero setup` and `zero auth`, and distinguish a
  missing key from a rejected key.
- `zero doctor` no longer reports "Overall: pass" when no provider credential is configured, and
  formats the missing-language-server list for humans (no raw Go `map[...]`).
- Raised the `faint`/`faintest` theme tokens (and the light-theme accent) to meet WCAG AA contrast for
  the content they carry.
- `NO_COLOR` is now honored for any non-empty value, per the no-color.org spec.

### Removed
- The inert `/input-style` slash command (it had no backend).

### Fixed
- README/`go.mod` Go-version mismatch and other stale public-release docs claims.
