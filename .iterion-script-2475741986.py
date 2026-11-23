import base64, calendar, json, os, re, subprocess, tempfile, time, urllib.parse, urllib.request, urllib.error
null = None; true = True; false = False  # JSON-literal shim for missing refs
pr_url = "https://github.com/SocialGouv/buildkit-operator/pull/49"
verdict = "hold_security"
audit_summary = "Renovate bumps the Go toolchain in two coordinated pins: go.mod `toolchain go1.26.11 -\u003e go1.27.3` and the devbox nix pin `go@1.26` (resolved 1.26.10, nixpkgs 4fe25e1…) -\u003e `go@1.27` (resolved 1.27.3, nixpkgs 93a9ed0…). Scope is exactly these three files (no_manifest=false, degraded=false; nothing inferred). Supply chain is clean: both nixpkgs commits verified on NixOS/nixpkgs with committer dates matching the lock's last_modified stamps, and 93a9ed0 verifiably carries version = 1.27.3 in pkgs/development/compilers/go/1.27.nix; no MAL-/typosquat/yanked records on stdlib or toolchain ids (both ids confirmed known in OSV); nix packages have no install-hook surface. The problem is vulnerabilities: OSV records for GO-2026-6848..6859 all carry the same ranges — {introduced 0, fixed 1.26.11} and {introduced 1.27.0-0, fixed 1.27.4} — so the old effective toolchain 1.26.11 (the go.mod directive governs which toolchain compiles the module, devbox 1.26.10 being merely the bootstrap) is clean, while the new pin 1.27.3 is affected by all twelve. Eleven are the Go security batch CVE-2026-97033..97043 (cgo/compiler/linker flag injection and build-time arbitrary code execution in cmd/go, cmd/cgo, cmd/compile, cmd/link), NVD-scored 8.3-8.8 HIGH (three of them unscored in my NVD queries due to rate-limit responses; same batch/class, counted HIGH per the unscored rule). The twelfth, CVE-2026-97054 (5.9 MEDIUM), is an HTTP/1 server crash via a MaxBytesReader header race that govulncheck confirms is reachable from this repo's own code — 44 symbol traces across cmd/gateway, cmd/buildd, internal/identity and the controllers. osv-scanner (2.4.0) reads the module dependencies only (go.sum untouched, 71 packages, zero findings on both old and new pins and repo-wide incl. pnpm-lock.yaml) and does not read the toolchain directive, so the toolchain coverage is the live OSV API queries plus NVD; the preinstalled govulncheck binary could not run under go1.27.3 (built with go1.26 source-processing packages: 'unknown field rfd in struct literal of type splicePipe'; and no gcc in the sandbox for cgo), so it was rebuilt with CGO_ENABLED=0 and run that way — matching this repo's own documented build mode. Verdict suspicious rather than safe: the bump INTRODUCES eleven HIGH advisories plus one reachable MEDIUM versus a clean old pin. Holding costs nothing — the fix release go1.27.4 is already stable on go.dev/dl (Renovate lists it as Pending), and it resolves all twelve records; the right action for a human is to skip this PR and take the 1.27.4 bump for both go.mod and devbox (nixpkgs may lag the 2026-11-21 lock snapshot by a few days for 1.27.4). For completeness: the devbox-only view is REMAINING — the dev shell's own 1.26.10 bootstrap was already inside the affected 'introduced 0' range for all twelve records both before and after this PR, so this bump does not worsen the dev shell; it is the go.mod directive change that regresses the effective toolchain."
cves = ["INTRODUCED GO-2026-6848 / CVE-2026-97043 (toolchain, cmd/go+cmd/compile): cgo_ldflag escaped-quote code execution — NVD 8.8 HIGH; affects 1.27.0–1.27.3, not 1.26.11 (fixed 1.26.11/1.27.4)","INTRODUCED GO-2026-6849 / CVE-2026-97042 (toolchain, cmd/cgo+cmd/compile+cmd/link): cgo_import_dynamic PE DLL linker-flag injection — NVD record present (query unparseable); same batch/class, counts HIGH; affects 1.27.0–1.27.3 only","INTRODUCED GO-2026-6850 / CVE-2026-97041 (toolchain, cmd/compile+cmd/link): //go:cgo_* host linker-flag injection — NVD record present (query unparseable); counts HIGH; affects 1.27.0–1.27.3 only","INTRODUCED GO-2026-6851 / CVE-2026-97040 (toolchain, cmd/go+cmd/link): .syso-disguised-as-object cgo directive injection — NVD record present (query unparseable); counts HIGH; affects 1.27.0–1.27.3 only","INTRODUCED GO-2026-6852 / CVE-2026-97039 (toolchain, cmd/go): #cgo pkg-config flag-sanitization bypass — NVD 8.3 HIGH; affects 1.27.0–1.27.3, not 1.26.11","INTRODUCED GO-2026-6853 / CVE-2026-97038 (toolchain, cmd/go): crafted .syso / cgo linker inputs -\u003e arbitrary code execution at link time — NVD 8.8 HIGH; affects 1.27.0–1.27.3, not 1.26.11","INTRODUCED GO-2026-6854 / CVE-2026-97037 (toolchain, cmd/go): split -Wl,/Darwin linker-flag injection in cgo — NVD 8.8 HIGH; affects 1.27.0–1.27.3, not 1.26.11","INTRODUCED GO-2026-6855 / CVE-2026-97036 (toolchain, cmd/go, darwin-only): cgo -l to_library build-time code execution — NVD 8.8 HIGH; affects 1.27.0–1.27.3, not 1.26.11","INTRODUCED GO-2026-6856 / CVE-2026-97035 (toolchain, cmd/go): Clang -gen-*/GCC -gnat* flag code execution + arbitrary file write — NVD 8.8 HIGH; affects 1.27.0–1.27.3, not 1.26.11","INTRODUCED GO-2026-6857 / CVE-2026-97034 (toolchain, cmd/go): GCC spec files in cgo linker search dirs -\u003e arbitrary command execution — NVD 8.8 HIGH; affects 1.27.0–1.27.3, not 1.26.11","INTRODUCED GO-2026-6858 / CVE-2026-97033 (toolchain, cmd/go): --sysroot/-target/-isysroot/-x cgo flag code execution — NVD 8.8 HIGH; affects 1.27.0–1.27.3, not 1.26.11","INTRODUCED GO-2026-6859 / CVE-2026-97054 (stdlib, net/http): MaxBytesReader Connection:close header race -\u003e HTTP/1 server crash — NVD 5.9 MEDIUM; govulncheck confirms 44 reachable symbol traces in this repo (cmd/gateway health server, OIDC verifier, controllers); affects 1.27.0–1.27.3, not 1.26.11","REMAINING GO-2026-6848..6859 (same 12 records) relative to the devbox nix pin go 1.26.10: 1.26.10 sits in the 'introduced 0, fixed 1.26.11' range, so the dev-shell bootstrap Go was already affected by all 12 before and after this bump — this view is the devbox triple only; the effective build toolchain (go.mod toolchain directive) goes clean(1.26.11) -\u003e affected(1.27.3)","RESOLVED none — no advisory affects the old effective toolchain 1.26.11; osv-scanner reports zero advisories on the 71 module dependencies (go.sum unchanged by this PR)"]
malware_signals = []
align_summary = null
validate_summary = null
escalation = null
commit_summary = null
audited_sha = "2a29155a8fb7e34cb3c7dce45a9e45038be114a3"
token_ref = "/run/iterion/secrets/forge_token"
pub_url = "https://iterion.cloud/api/v1/forge/publish-review"
run_url = "https://iterion.cloud/studio/runs/01a20642-853b-75cb-a8a7-0c37cbdb3a1b"
run_id = "01a20642-853b-75cb-a8a7-0c37cbdb3a1b"
# The grant reaches this script through the interpreter's ENVIRONMENT,
# under the engine's placeholder for the var: a script body is written to
# a temp file (and pushed into the workspace by a copy-based sandbox), so
# an inlined bearer would sit in clear on disk for the node's lifetime.
pub_token = os.environ.get("__ITERION_SECRET_forge_publish_token__", "")
gate_enabled = true
gate_context = "revi/review"
origin_sha = null
origin_at = null
base_ref = "main"
head_sha = "2a29155a8fb7e34cb3c7dce45a9e45038be114a3"
head_id = "pid1:bfaeb070121b38c4707d843a"
continuity_on = true
carry_max_age_h = 72

def s(x):
    return (x or "").strip() if isinstance(x, str) else ("" if x is None else str(x).strip())

# pinned_sha: the revision the verdict is finally about — the audited
# commit, or the head a repin carried it onto. arm_automerge vouches the
# gate against it.
def out(posted, comment_url="", skipped_reason="", summary="",
        gate_posted=False, gate_state="", gate_skipped_reason="", gate_sha=""):
    print(json.dumps({"verdict": s(verdict),
                      "posted": bool(posted), "comment_url": comment_url or "",
                      "skipped_reason": skipped_reason or "", "summary": summary or "",
                      "gate_posted": bool(gate_posted), "gate_state": gate_state or "",
                      "gate_skipped_reason": gate_skipped_reason or "",
                      "gate_sha": gate_sha or "", "pinned_sha": s(audited_sha)}))
    raise SystemExit(0)

if not s(pr_url):
    out(False, skipped_reason="no pr_url (local run)")

# >>> vetty-change-identity/v1
# Byte-identical in `continuity` and `post_feedback`: the id one stamps is
# the id the other compares (dep_update_guard_gate_test.go pins the two
# copies). The identity of the change a head carries is the verbatim
# patch-id of merge-base(base_rev, sha)..sha, every diff knob pinned on the
# command line, folded with the base_ref NAME so a retargeted pull request
# never matches. A rebase that leaves the hunks and their three context
# lines alone keeps it; a conflict always touches one of them, so it
# changes it. --verbatim, not --stable: --stable strips whitespace and
# reads a re-indented YAML block as the same change. Only the commit
# objects decide: replace refs and grafts are switched off, and attributes
# are read from the empty tree, so neither a .gitattributes in the change
# nor the settings of the repository it is computed in move the id.
# Returns (id, reason); the id is empty when no identity exists, and the
# reason says why.
def change_identity(ws, base_ref, base_rev, sha):
    import hashlib, subprocess
    # git's built-in empty tree: the only attribute source read.
    empty = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"
    # Variables that point git at another repository or object store, or
    # inject diff settings this contract pins. GIT_CONFIG_COUNT stays: the
    # sandbox carries safe.directory there, and the flags below override
    # every diff setting it could hold.
    drop = ("GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY",
            "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_COMMON_DIR", "GIT_NAMESPACE",
            "GIT_LITERAL_PATHSPECS", "GIT_GLOB_PATHSPECS", "GIT_NOGLOB_PATHSPECS",
            "GIT_ICASE_PATHSPECS", "GIT_EXTERNAL_DIFF", "GIT_DIFF_OPTS", "GIT_CONFIG",
            "GIT_CONFIG_PARAMETERS", "GIT_ATTR_SOURCE")
    env = dict((k, v) for k, v in os.environ.items() if k not in drop)
    # GIT_GRAFT_FILE names a path that cannot exist: no graft is read,
    # and git prints no deprecation notice for a file it never opens.
    env.update({"GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": os.devnull,
                "GIT_ATTR_NOSYSTEM": "1", "GIT_TERMINAL_PROMPT": "0", "LC_ALL": "C",
                "GIT_NO_REPLACE_OBJECTS": "1", "GIT_GRAFT_FILE": os.path.join(os.devnull, "no-grafts"),
                "GIT_ATTR_SOURCE": empty})
    def git(args, data=None):
        return subprocess.run(["git", "-C", ws, "-c", "core.quotePath=true",
                               "-c", "core.attributesFile=" + os.devnull, "-c", "attr.tree=" + empty,
                               "-c", "diff.suppressBlankEmpty=false"] + args,
                              input=data, capture_output=True, timeout=120, env=env)
    def commit(rev):
        p = git(["rev-parse", "--verify", "--quiet", "--end-of-options", rev + "^{commit}"])
        return p.stdout.decode().strip() if p.returncode == 0 else ""
    try:
        if not ws or not base_ref or not base_rev or not sha:
            return "", "no change identity: the workspace, the base or the head is missing"
        head, base = commit(sha), commit(base_rev)
        if not head or not base:
            return "", "no change identity: " + (base_rev if head else sha[:12]) + " does not resolve to a commit"
        p = git(["merge-base", base, head])
        mb = p.stdout.decode().strip()
        if p.returncode != 0 or not mb:
            return "", "no change identity: no merge-base between " + base_rev + " and " + head[:12]
        # The path order is pinned (-O with an empty order file overrides
        # diff.orderFile): a binary section makes the patch-id sum depend on
        # the order of the files around it.
        p = git(["diff", "--no-color", "--no-ext-diff", "--no-textconv", "--no-renames",
                 "--full-index", "--unified=3", "--inter-hunk-context=0",
                 "--diff-algorithm=myers", "--indent-heuristic", "--no-relative",
                 "--ignore-submodules=none", "--submodule=short",
                 "--src-prefix=a/", "--dst-prefix=b/", "-O" + os.devnull, mb, head, "--"])
        if p.returncode != 0:
            return "", "no change identity: git diff " + mb[:12] + ".." + head[:12] + " failed"
        if not p.stdout:
            return "", "no change identity: " + head[:12] + " changes nothing against its base"
        q = git(["patch-id", "--verbatim"], p.stdout)
        pid = (q.stdout.decode().split() + [""])[0] if q.returncode == 0 else ""
        if not pid:
            v = git(["version"]).stdout.decode().strip() or "this git"
            return "", "no change identity: " + v + " cannot compute patch-id --verbatim (git 2.39 or later)"
        # patch-id hashes no path for a section with no ---/+++ lines (a
        # mode change, an empty file, a binary) that follows a binary one.
        # The modes, the status and the path of EVERY changed entry are
        # folded in from the raw listing, blob ids left out: a rebase that
        # moves a changed file's base content elsewhere keeps the id.
        r = git(["diff", "--raw", "-z", "--no-abbrev", "--no-renames", "--no-relative",
                 "--ignore-submodules=none", "-O" + os.devnull, mb, head, "--"])
        entries = r.stdout.split(b"\0") if r.returncode == 0 else []
        if not entries or entries.pop() != b"" or len(entries) % 2:
            return "", "no change identity: the changed paths of " + mb[:12] + ".." + head[:12] + " cannot be read"
        listing = b""
        for meta, path in zip(entries[0::2], entries[1::2]):
            f = meta.split(b" ")
            if len(f) != 5 or not f[0].startswith(b":") or not path:
                return "", "no change identity: the changed paths of " + mb[:12] + ".." + head[:12] + " cannot be read"
            listing += f[0][1:] + b" " + f[1] + b" " + f[4] + b"\t" + path + b"\0"
        digest = hashlib.sha256(b"vetty-change/v1\0" + base_ref.encode() + b"\0" + pid.encode() + b"\0" +
                                listing).hexdigest()
        return "pid1:" + digest[:24], "patch-id of " + mb[:12] + ".." + head[:12]
    except (OSError, subprocess.SubprocessError, UnicodeError) as e:
        return "", "no change identity: git could not run (" + type(e).__name__ + ")"
# <<< vetty-change-identity/v1

# >>> vetty-gate-history/v1
# Byte-identical in `continuity` and `post_feedback`
# (dep_update_guard_continuity_test.go pins the two copies): ONE reading
# of the gate history of ONE change on the pull request, and the predicate
# that lets continuity carry a verdict onto a head and post_feedback repin
# or publish one there. It reads, with the run token, who that token
# speaks as (the GraphQL viewer), every review with its edit record
# (GraphQL), the last-line marker each carries, and the forge's one status
# for the context on every sha a marker names AND on the head, with the
# identity that wrote it — and decides over SETS, never over an order: the
# forge pages reviews in whatever order it likes. `call(path)` is a REST
# read returning (json, Link header), `gql(query, variables)` a GraphQL
# read returning its data; both raise on a failed read.
#
# This App is the identity the run token speaks as — the server publishes
# the gate under the same installation — told by user id alone: REST and
# GraphQL spell a bot's login differently (`slug[bot]` / `slug`), and a
# user account can register any spelling. A token with no id owns
# nothing. A review this App published and somebody edited since — anyone
# with write access can, and the author stays the App — no longer says
# what was published: it vetoes, whatever it says now. A marker vetoes
# whoever wrote it, the safe direction. A status stamped with the change
# counts only when this App wrote it, and vetoes when anybody else did.
#
# Returns (veto, eligible). veto says why no green may be carried onto
# `head`: a history not read whole, an edited review of this App, a
# verdict on this change that is not green, a status stamped with it that
# is not green or not this App's, a red status on the head itself (a carry
# never paints over one). eligible lists this App's unedited green
# verdicts on the change whose status — written by this App — the forge
# holds green, stamped with the change and linking that review.
def gate_history(call, gql, owner, repo, num, pid, gctx, head):
    green = ("clean", "committed", "carried")
    marker = re.compile(r"<!-- vetty-gate/v1 sha=([0-9a-f]{40}) id=(pid1:[0-9a-f]{24}) verdict=([a-z_]+) "
                        r"origin=([0-9a-f]{40}) origin_at=([0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z) -->")
    pages = 10
    def text(x):
        return x.strip() if isinstance(x, str) else ""
    def user_id(x):
        return x if isinstance(x, int) and not isinstance(x, bool) else None
    uid = user_id(((gql("query { viewer { login databaseId } }", {}).get("viewer")) or {}).get("databaseId"))
    # The variable sigil is written @ and swapped at run time.
    reviews_q = ("query(@owner: String!, @name: String!, @number: Int!, @after: String) { "
                 "repository(owner: @owner, name: @name) { pullRequest(number: @number) { "
                 "reviews(first: 100, after: @after) { pageInfo { hasNextPage endCursor } nodes { "
                 "url body lastEditedAt editor { login } "
                 "author { login ... on Bot { databaseId } ... on User { databaseId } } "
                 "} } } } }").replace("@", chr(36))
    reviews, after = [], None
    for _ in range(pages):
        got = (((gql(reviews_q, {"owner": owner, "name": repo, "number": int(num), "after": after})
                 .get("repository") or {}).get("pullRequest") or {}).get("reviews"))
        if not isinstance(got, dict) or not isinstance(got.get("nodes"), list):
            return "the review history is unreadable", []
        reviews.extend(got["nodes"])
        info = got.get("pageInfo") if isinstance(got.get("pageInfo"), dict) else {}
        more, after = info.get("hasNextPage"), info.get("endCursor")
        if more is False:
            break
        if more is not True or not text(after):
            return "the review history is unreadable (a page names no next one)", []
    else:
        return "the review history runs past " + str(pages) + " pages, so it cannot be read whole", []
    marks, edited = [], []
    for r in reviews:
        if not isinstance(r, dict) or "lastEditedAt" not in r:
            return "the review history is unreadable (a review without its edit record)", []
        author = r.get("author") if isinstance(r.get("author"), dict) else {}
        own = uid is not None and user_id(author.get("databaseId")) == uid
        if own and r.get("lastEditedAt") is not None:
            editor = r.get("editor") if isinstance(r.get("editor"), dict) else {}
            edited.append((text(r.get("url")), text(r.get("lastEditedAt")), text(editor.get("login"))))
        lines = text(r.get("body")).splitlines()
        mk = marker.fullmatch(lines[-1].strip()) if lines else None
        if mk:
            marks.append({"sha": mk.group(1), "id": mk.group(2), "verdict": mk.group(3),
                          "origin": mk.group(4), "origin_at": mk.group(5), "own": own,
                          "url": text(r.get("url"))})
    if edited:
        url, when, who = min(edited)
        return ("a review this App published was edited after it was created (" + (url or "a review") + ", by " +
                (who or "an editor the forge does not name") + " at " + (when or "an unknown time") +
                "): the history no longer says what was published, and a tampered history carries nothing"), []
    same = sorted((k for k in marks if k["id"] == pid),
                  key=lambda k: (k["sha"], k["verdict"], k["origin_at"], k["origin"], k["url"]))
    # Unanimity: one verdict on this change that is not green vetoes every
    # carry, whatever greens sit beside it. It covers the hold whose gate
    # the server refused because the head moved under it.
    held = [k for k in same if k["verdict"] not in green]
    if held:
        return ("a verdict on this same change (" + pid + ") holds: " + held[0]["verdict"] + " on " +
                held[0]["sha"][:12] + ", and a carry needs every verdict on it green"), []
    if uid is None:
        return "the run token speaks as an identity with no user id, so no verdict or status can be told as this App's", []
    stamp = " · " + pid
    statuses = {}
    for sha in sorted(set([k["sha"] for k in same] + [head])):
        combined = call("/repos/%s/%s/commits/%s/status?per_page=100" % (owner, repo, sha))[0]
        listed = combined.get("statuses") if isinstance(combined, dict) else None
        if not isinstance(listed, list) or (isinstance(combined.get("total_count"), int) and
                                            combined["total_count"] > len(listed)):
            return "the statuses on " + sha[:12] + " cannot be read whole", []
        statuses[sha] = None
        for st in listed:
            if isinstance(st, dict) and text(st.get("context")) == gctx:
                statuses[sha] = st
                break
    # Who wrote the status the forge holds current: the combined status
    # names it by id only, so it is found by that id in the commit's
    # status list, which carries its creator. None when it cannot be.
    def written_by(sha, st):
        sid = user_id(st.get("id"))
        if sid is None:
            return None
        for page in range(1, pages + 1):
            listed, link = call("/repos/%s/%s/commits/%s/statuses?per_page=100&page=%d" % (owner, repo, sha, page))
            if not isinstance(listed, list):
                return None
            for x in listed:
                if isinstance(x, dict) and user_id(x.get("id")) == sid:
                    return user_id((x.get("creator") if isinstance(x.get("creator"), dict) else {}).get("id"))
            if 'rel="next"' not in link:
                return None
        return None
    for sha in sorted(statuses):
        st = statuses[sha]
        if st is None:
            continue
        state = text(st.get("state")) or "unset"
        stamped = text(st.get("description")).endswith(stamp)
        if stamped and state != "success":
            return "the " + gctx + " status on " + sha[:12] + " for this same change (" + pid + ") is " + state, []
        if sha == head and state not in ("success", "pending"):
            return "the " + gctx + " status on this head (" + sha[:12] + ") is " + state + ", and a carry never paints over it", []
        if stamped:
            who = written_by(sha, st)
            if who is None:
                return "the author of the " + gctx + " status on " + sha[:12] + " cannot be read", []
            if who != uid:
                return ("the " + gctx + " status on " + sha[:12] + " carries this change's stamp (" + pid +
                        ") but another identity wrote it (user id " + str(who) + "): only this App's stamp is a "
                        "source, and a foreign one vetoes"), []
    def eligible(k):
        st = statuses.get(k["sha"])
        return (k["own"] and k["verdict"] in green and st is not None and bool(k["url"])
                and text(st.get("state")) == "success" and text(st.get("description")).endswith(stamp)
                and text(st.get("target_url")) == k["url"])
    return "", [k for k in same if eligible(k)]
# <<< vetty-gate-history/v1

# The verdicts a gate may carry onto another head of the same change.
GREEN = ("clean", "committed", "carried")
FULL_SHA = re.compile(r"[0-9a-f]{40}")
GITHUB_API = "https://api.github.com"
# Where the forge serves the repository over git. The fetch below names
# it explicitly: the workspace's own remote and url rewrites are never
# read.
GITHUB_GIT = "https://github.com/"
pull_ref = re.match(r"https?://([^/]+)/([^/]+)/([^/]+)/(?:pull|pulls)/([0-9]+)(?:[/?#]|$)", s(pr_url))
on_github = bool(pull_ref) and pull_ref.group(1).lower() == "github.com"
branch_ok = bool(re.fullmatch(r"[A-Za-z0-9._/-]+", s(base_ref))) and not s(base_ref).startswith("-") and \
    ".." not in s(base_ref)

def read_token():
    tref = s(token_ref)
    if not tref:
        return ""
    return (open(tref).read().strip() if os.path.exists(tref) else tref)

def call(path, body=None):
    # A read of the GitHub API with the run's forge token — a REST GET, or
    # a GraphQL query POSTed with `body` — never a write, never a redirect
    # followed.
    class _Stay(urllib.request.HTTPRedirectHandler):
        def redirect_request(self, *args, **kwargs):
            return None
    req = urllib.request.Request(
        GITHUB_API + path, data=body, method="POST" if body is not None else "GET",
        headers={"Authorization": "Bearer " + read_token(), "Accept": "application/vnd.github+json",
                 "Content-Type": "application/json", "User-Agent": "vetty-dep-update-guard"})
    with urllib.request.build_opener(_Stay()).open(req, timeout=30) as resp:
        return json.loads(resp.read().decode("utf-8")), resp.headers.get("Link") or ""

def gql(query, variables):
    data = call("/graphql", json.dumps({"query": query, "variables": variables}).encode("utf-8"))[0]
    if not isinstance(data, dict) or data.get("errors"):
        raise ValueError("the GraphQL read answered errors")
    return data.get("data") or {}

# >>> vetty-required-checks/v1
# Byte-identical in `continuity`, `post_feedback` and `arm_automerge`
# (dep_update_guard_continuity_test.go pins the three copies). The check
# rollup of the pull request's head, read with GraphQL — `gql(query,
# variables)` returns the data and raises on a failed read — as (required,
# optional, passing): the names of the contexts not passing on `pin`,
# split by whether the base requires them for this pull request, and the
# names of the required ones that pass. None when the rollup is about
# another commit than `pin`, runs past one page, or carries a context
# whose kind or requiredness it does not state: what cannot be read whole
# proves nothing. The variable sigil is written @ and swapped at run time.
def check_rollup(gql, owner, repo, num, pin):
    def text(x):
        return x.strip() if isinstance(x, str) else ""
    q = ("query(@owner: String!, @name: String!, @number: Int!) { "
         "repository(owner: @owner, name: @name) { "
         "pullRequest(number: @number) { commits(last: 1) { nodes { commit { oid "
         "statusCheckRollup { contexts(first: 100) { pageInfo { hasNextPage } nodes { __typename "
         "... on StatusContext { context state isRequired(pullRequestNumber: @number) } "
         "... on CheckRun { name status conclusion isRequired(pullRequestNumber: @number) } "
         "} } } } } } } } }").replace("@", chr(36))
    data = gql(q, {"owner": owner, "name": repo, "number": int(num)})
    nodes = ((((data.get("repository") or {}).get("pullRequest") or {}).get("commits") or {}).get("nodes")) or []
    if len(nodes) != 1 or not isinstance(nodes[0], dict):
        return None
    commit = nodes[0].get("commit") or {}
    # The pin may be abbreviated (7 hex at least): the rollup's commit is
    # compared on the common prefix.
    oid, want = text(commit.get("oid")).lower(), text(pin).lower()
    n = min(len(oid), len(want))
    if n < 7 or oid[:n] != want[:n]:
        return None
    contexts = (commit.get("statusCheckRollup") or {}).get("contexts") or {}
    if (contexts.get("pageInfo") or {}).get("hasNextPage") or not isinstance(contexts.get("nodes"), list):
        return None
    required, optional, passing = [], [], []
    for c in contexts["nodes"]:
        if not isinstance(c, dict):
            return None
        kind = text(c.get("__typename"))
        if kind == "StatusContext":
            name, ok = text(c.get("context")), text(c.get("state")).upper() == "SUCCESS"
        elif kind == "CheckRun":
            name = text(c.get("name"))
            ok = text(c.get("status")).upper() == "COMPLETED" and \
                text(c.get("conclusion")).upper() in ("SUCCESS", "NEUTRAL", "SKIPPED")
        else:
            return None
        if ok:
            # Counted only when the base states it requires it.
            if c.get("isRequired") is True:
                passing.append(name)
            continue
        if c.get("isRequired") is True:
            required.append(name)
        elif c.get("isRequired") is False:
            optional.append(name)
        else:
            return None
    return sorted(required), sorted(optional), sorted(passing)
# A carried verdict attests no build: the build of the identical change on
# this base is owed by a check the base REQUIRES other than the gate's own
# context — the forge then holds every merge of the pull request to it.
# "" when such a check is on `pin`, in any state: an arming or an enqueue
# waits for it to pass, and CLEAN or UNSTABLE means it passed. Otherwise
# the refusal, naming the gate context.
def build_owed(rollup, gctx, pin):
    if rollup is None:
        return ("the check rollup on " + pin[:12] + " could not be read whole, so nothing says a check the base "
                "requires other than the gate (" + gctx + ") builds this carried change")
    required, _, passing = rollup
    if [c for c in required + passing if c != gctx]:
        return ""
    return ("a carried verdict attests no build, and the base requires no check other than the gate (" + gctx +
            ") on " + pin[:12] + ": nothing would build this head before it merges")
# <<< vetty-required-checks/v1

def forge_identity(sha):
    # The change identity of `sha`, for a revision the AGENTS could have
    # touched: computed in a repository this node creates now, outside the
    # workspace, and fills from the forge alone. The workspace is never
    # read here — the agents ran in it with a shell, and its refs,
    # replace objects, grafts, config and object store are theirs to
    # rewrite. The scratch repository's config is cut off from the system
    # and global ones, the URL is named explicitly, and every object it
    # holds was hashed by index-pack on receipt: the pull request's base
    # branch as the forge has it NOW, and `sha`. Shallow, deepened once
    # when no merge-base is in reach. The token rides git's environment
    # (GIT_CONFIG_COUNT/KEY/VALUE), never argv, and nothing git says is
    # printed. Returns (id, reason).
    if not on_github or not branch_ok or not FULL_SHA.fullmatch(sha or ""):
        return "", "no forge identity: a GitHub pull request, a plain base branch and a full commit id are needed"
    url = GITHUB_GIT + pull_ref.group(2) + "/" + pull_ref.group(3) + ".git"
    drop = ("GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY",
            "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_COMMON_DIR", "GIT_NAMESPACE",
            "GIT_CONFIG", "GIT_CONFIG_PARAMETERS", "GIT_ASKPASS", "SSH_ASKPASS")
    env = dict((k, v) for k, v in os.environ.items() if k not in drop)
    env.update({"GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": os.devnull, "GIT_TERMINAL_PROMPT": "0",
                "GIT_NO_REPLACE_OBJECTS": "1", "GIT_GRAFT_FILE": os.path.join(os.devnull, "no-grafts")})
    token = read_token()
    try:
        if url.startswith("https://") and token:
            n = int(env.get("GIT_CONFIG_COUNT") or "0")
            env["GIT_CONFIG_KEY_%d" % n] = "http.https://" + urllib.parse.urlsplit(url).netloc + "/.extraHeader"
            env["GIT_CONFIG_VALUE_%d" % n] = "Authorization: Basic " + \
                base64.b64encode(("oauth2:" + token).encode()).decode()
            env["GIT_CONFIG_COUNT"] = str(n + 1)
        with tempfile.TemporaryDirectory(prefix="vetty-identity-") as scratch:
            def git(args):
                return subprocess.run(["git", "-C", scratch] + args, capture_output=True, timeout=300, env=env)
            if git(["init", "-q", "--bare"]).returncode != 0:
                return "", "no forge identity: git could not create a scratch repository"
            wanted = ["+refs/heads/" + s(base_ref) + ":refs/vetty/base", "+" + sha + ":refs/vetty/head"]
            for depth in (64, 1024):
                p = git(["-c", "http.followRedirects=false", "fetch", "--no-tags", "--quiet",
                         "--depth=%d" % depth, url] + wanted)
                if p.returncode != 0:
                    return "", "no forge identity: the forge did not serve " + sha[:12] + " and " + \
                        s(base_ref) + " (git fetch exit " + str(p.returncode) + ")"
                mb = git(["merge-base", "refs/vetty/base", "refs/vetty/head"])
                if mb.returncode == 0 and mb.stdout.strip():
                    return change_identity(scratch, s(base_ref), "refs/vetty/base", sha)
            return "", "no forge identity: no merge-base of " + sha[:12] + " within 1024 commits of " + s(base_ref)
    except (OSError, ValueError, subprocess.SubprocessError, UnicodeError) as e:
        return "", "no forge identity: git could not run (" + type(e).__name__ + ")"

# What the gate stamps: the change identity of the revision this verdict is
# pinned to, and only when continuity identified the head prepare read
# BEFORE any agent ran — the audit then read the true change. On every
# edge but one the pin is that head, and its identity is continuity's; on
# the did_commit edge the pin is the alignment this run pushed, identified
# from the forge (forge_identity), never the workspace. Only a FULL commit
# id is stamped, and only on a gate this run can publish; a pin with no
# identity publishes as before, with no stamp and no marker, and so can
# never be carried.
pin_sha = s(audited_sha).lower()
read_head = s(head_sha).lower()
read_id = s(head_id) if re.fullmatch(r"pid1:[0-9a-f]{24}", s(head_id)) else ""
origin = s(origin_sha).lower() if FULL_SHA.fullmatch(s(origin_sha).lower()) else ""
origin_when = s(origin_at) if re.fullmatch(r"[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z", s(origin_at)) else ""
publish_gate = bool(gate_enabled) and bool(s(pub_url)) and bool(s(pub_token))
change_id, id_note = "", ""
if publish_gate and FULL_SHA.fullmatch(pin_sha):
    if not read_id:
        id_note = "unstamped: continuity computed no identity for the head this run read"
    elif pin_sha == read_head:
        change_id = read_id
    else:
        change_id, id_note = forge_identity(pin_sha)

# REUSE (green verdicts only). A verdict this run did not audit on the
# revision it is published on — `carried` from continuity, or a green
# repinned below onto a head the audit never read — is published only
# when the gate history, read again RIGHT before the publish, still
# vetoes nothing about that revision's change (one reading, the block
# continuity decides with), and a check the base requires other than the
# gate builds that revision: a carried verdict attests no build. The
# history is read at publish time because the one continuity read can
# be minutes old — hours on a resumed run — and a hold published on any
# head of the change since then vetoes every later carry. A carry
# continuity decided that the rule no longer lets through is withdrawn:
# nothing is published, the run's gate claim stays unanswered, and the
# reason says why. The server orders decisions by
# the time it receives them, so a hold published after this read still
# supersedes the carry; only one landing in the second between the read
# and the publish is overwritten, a window only a compare-and-set on the
# publish endpoint would close.
#
# REPIN. A push that lands while the audit runs moves the head, and the
# server refuses a verdict pinned to the old one. When the new head
# carries the IDENTICAL change — its identity read from the forge, never
# from the workspace — and the rule above lets a carry onto it, the
# verdict is published there as `carried`, its origin the commit actually
# audited. Anything else publishes exactly as before, pinned to the
# audited commit, and is refused for the moved head. A hold is never
# moved: it is a statement about the commit it read, and nothing else.
#
# The operator's switches govern every reuse exactly as they govern
# continuity's: `continuity` off (the /vetty command, or an operator pin)
# or carry_max_age_h at 0 reuses nothing, and a carried verdict whose
# ORIGINAL audit is older than carry_max_age_h is not reused.
try:
    limit_h = float(carry_max_age_h or 0)
except (TypeError, ValueError):
    limit_h = 0.0
reuse_off = ""
if not bool(continuity_on):
    reuse_off = "continuity is off for this launch"
elif limit_h <= 0:
    reuse_off = "carry_max_age_h is " + (s(carry_max_age_h) or "unset")
elif origin_when and (time.time() - calendar.timegm(time.strptime(origin_when, "%Y-%m-%dT%H:%M:%SZ"))) / 3600.0 > limit_h:
    reuse_off = "the original audit (" + origin_when + ") is older than carry_max_age_h=" + s(carry_max_age_h) + "h"
gctx = s(gate_context) or "vetty/deps"

def carry_vetoed(owner, repo, num, pid, onto):
    # Why a green verdict may not be published as carried onto `onto`,
    # "" when it may.
    veto, _ = gate_history(call, gql, owner, repo, num, pid, gctx, onto)
    return veto or build_owed(check_rollup(gql, owner, repo, num, onto), gctx, onto)

# The carry continuity decided must still hold at publish time.
withdrawn = ""
if publish_gate and s(verdict) == "carried":
    if not (change_id and origin and origin_when):
        withdrawn = "the carried verdict names no change identity or no original audit"
    elif reuse_off:
        withdrawn = reuse_off
    elif not on_github or not read_token():
        withdrawn = "the gate history cannot be read again (a GitHub pull request and a forge_token are needed)"
repinned_from, repin_note = "", ""
if change_id and s(verdict) in GREEN and on_github and reuse_off:
    repin_note = "no repin: " + reuse_off
elif change_id and s(verdict) in GREEN and on_github and read_token() and not withdrawn:
    owner, repo, num = pull_ref.group(2), pull_ref.group(3), pull_ref.group(4)
    try:
        pull = call("/repos/%s/%s/pulls/%s" % (owner, repo, num))[0]
        head_now = s((pull.get("head") or {}).get("sha")).lower()
        base_now = s((pull.get("base") or {}).get("ref"))
        moved = bool(FULL_SHA.fullmatch(head_now)) and head_now != pin_sha
        if moved and base_now != s(base_ref):
            repin_note = "not repinned: the pull request now targets " + (base_now or "(no base)")
        elif moved:
            head_id, why = forge_identity(head_now)
            if head_id != change_id:
                repin_note = "not repinned onto " + head_now[:12] + ": " + \
                             (why if not head_id else "it carries another change (" + head_id + ")")
            else:
                veto = carry_vetoed(owner, repo, num, head_id, head_now)
                if veto:
                    repin_note = "not repinned onto " + head_now[:12] + ": " + veto
                else:
                    repinned_from = pin_sha
                    if s(verdict) != "carried":
                        origin, origin_when = pin_sha, time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
                    verdict = "carried"
                    audited_sha, pin_sha, change_id = head_now, head_now, head_id
                    repin_note = "repinned from " + repinned_from[:12] + " onto " + head_now[:12] + ", the identical change"
    except urllib.error.HTTPError as e:
        repin_note = "not repinned: the forge answered HTTP " + str(e.code)
    except Exception as e:
        repin_note = "not repinned: the forge could not be read (" + type(e).__name__ + ")"
# A carry continuity decided and no repin moved: read again before it is
# published on the head it was decided for.
if publish_gate and s(verdict) == "carried" and not repinned_from and not withdrawn:
    owner, repo, num = pull_ref.group(2), pull_ref.group(3), pull_ref.group(4)
    try:
        withdrawn = carry_vetoed(owner, repo, num, change_id, pin_sha)
    except urllib.error.HTTPError as e:
        withdrawn = "the gate history could not be read again (forge HTTP " + str(e.code) + ")"
    except Exception as e:
        withdrawn = "the gate history could not be read again (" + type(e).__name__ + ")"
if withdrawn:
    out(False, skipped_reason="the carried verdict was withdrawn before publishing: " + withdrawn,
        summary="verdict=carried withdrawn, nothing published: " + withdrawn,
        gate_skipped_reason="the carried verdict was withdrawn before publishing: " + withdrawn)
carried_from = (origin or repinned_from or pin_sha)[:12]

badges = {
    "hold_security":  "\U0001F6D1 HOLD — supply-chain risk; not aligned/merged",
    "needs_decision": "\U0001F9ED Needs a human decision (architectural)",
    "hold_unstable":  "⚠️ HOLD — build/tests not green after alignment",
    "hold_lost_alignment": "\U0001F6D1 HOLD — the alignment for this bump is NOT on the branch",
    "hold_delivery_permission": "\U0001F6D1 HOLD — this bump's alignment edits a workflow file, and the delivery permission for it was refused",
    "committed":      "✅ Aligned — code updated on this branch; review & merge",
    "clean":          "✅ Safe bump — nothing left to align",
    "carried":        "✅ Carried — the identical change was audited; this base is built by the repo's CI",
}
parts = ["**" + badges.get(s(verdict), "ℹ️ Vetty review") + "**", ""]
def section(title, body):
    if s(body):
        parts.extend(["## " + title, s(body), ""])
sec = s(audit_summary)
extra = []
if s(cves): extra.append("CVEs: " + s(cves))
if s(malware_signals): extra.append("Malware signals: " + s(malware_signals))
if extra: sec = (sec + "\n\n" + "\n".join(extra)).strip()
section("Security & supply-chain", sec)
section("Code alignment", align_summary)
section("Reliability", validate_summary)
if s(verdict) == "needs_decision":
    section("Decision needed", escalation)
did = {"committed": "Committed the alignment to this branch.",
       "hold_security": "Held — no alignment, no commit.",
       "hold_unstable": "Held — build/tests not green.",
       "needs_decision": "Escalated for a human decision.",
       # No cross-reference to a section title: `section()` renders nothing
       # for an empty body, so citing one would dangle exactly when align
       # reported no summary — and a reader who cannot find the section
       # doubts the HOLD instead of acting on it.
       "hold_lost_alignment": "Held — the code alignment this run produced for the bump is NOT on this branch: "
                              "nothing was committed and the branch head did not move. Re-run Vetty on "
                              "this PR to redo it; merging as-is ships the bump without it.",
       # TWO causes reach this verdict, and both are ANSWERED refusals:
       # a permission the App does not hold (the preflight's verdict) and
       # a classic token short of a scope (the forge API's scope list).
       # Their cures differ, so the text carries both and points at the
       # refusal that tells them apart. That refusal always renders: the
       # edge into post_feedback maps the probe's reason through
       # align_summary behind a literal prefix, so "Code alignment" is
       # never the empty section on this verdict. A preflight that did
       # NOT answer is deliberately absent — it reaches none of the
       # published verdicts (post_feedback never runs on it; the value
       # it hands arm_automerge is preflight_unavailable), it ends the
       # run as FORGE_PREFLIGHT_UNAVAILABLE, and a relaunch cure offered
       # here would describe a hold that cannot happen.
       "hold_delivery_permission": "Held BEFORE pushing — the alignment this bump needs edits a GitHub "
                                   "workflow file, and the delivery credential was REFUSED the `workflows` "
                                   "permission when asked for it, so the push was not attempted (GitHub "
                                   "refuses an App push touching .github/workflows/** without it). "
                                   "Nothing was committed. The refusal above says which cure applies: a "
                                   "permission the App does not hold — approve the delivery permissions "
                                   "and refresh its token, then re-run Vetty on this PR; a classic token "
                                   "short of a scope — reissue it as the refusal names, then re-run "
                                   "Vetty. Either way the alignment can also be applied by hand.",
       "clean": "Nothing left to align.",
       # A carry asserts the supply-chain audit and the alignment of an
       # IDENTICAL change, never a build of this tree: the build on the
       # new base is owed by the repo's own required checks.
       "carried": ("Audited " + repinned_from[:12] + "; the head moved to " + pin_sha[:12] +
                   " with the identical change, so the verdict is carried there. " if repinned_from else "") +
                  "Carried the supply-chain audit of `" + carried_from + "` to this head: the change is " +
                  "identical (" + (change_id or "no identity") + "), so nothing was re-audited, aligned or " +
                  "committed here. The build on this base is the repo's own CI's to prove."}.get(s(verdict), "")
did = (s(commit_summary) + " " if s(commit_summary) else "") + did + \
    " **Vetty never merges past a required check** — whether this lands is the repo's own required checks' " \
    "decision, and only the exact commit reported above can be merged."
section("What Vetty did", did)
# The run that wrote this comment: the reader's way to the steps, the
# models and the cost behind the verdict, and the way into the platform
# for someone meeting this bot on their own repository. No public address
# configured (a local run) leaves the id alone, never a made-up host.
#
# A run id is CALLER-CHOSEN at launch and only screened for path
# traversal, so it may hold markdown: an id like `x](https://evil` would
# close this label early and publish a foreign link under Vetty's
# signature. Only a plain id becomes a label; anything else is dropped
# and the link keeps a fixed one (the URL itself is path-escaped).
# On an engine that predates `run.url` the reference is UNRESOLVED, and a
# script body renders a missing ref as the JSON `null` literal (shimmed to
# None above), never as the delimiter itself — so s() empties it here and
# the footer degrades to the id alone. The sibling command-body node of
# branch-improve-loop needs its own guard for this, because THERE the
# delimiter does survive into the rendering.
rid = s(run_id) if re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._:-]*", s(run_id) or "") else ""
foot = "_\U0001F916 [iterion](https://iterion.cloud)"
if s(run_url):
    label = "`" + rid + "`" if rid else "this run"
    foot += " · run [" + label + "](" + s(run_url) + ")"
elif rid:
    foot += " · run `" + rid + "`"
elif s(run_id):
    # No address AND an id no label can carry: name the run anyway —
    # a verdict with no identifier is the assertion this footer exists
    # to avoid. Inside a code span, with backticks stripped, the id
    # can neither close the span nor open a link.
    foot += " · run `" + s(run_id).replace(chr(96), "") + "`"
foot += " — Vetty, deterministic feedback"
parts.append(foot + "._")
# The marker's opener is defused wherever an agent-written section
# carries it: only the line this node writes LAST is a marker, and
# continuity reads nothing else.
body = re.sub(r"(?i)<!--(\s*)vetty-gate", r"&lt;!--\1vetty-gate", "\n".join(parts))

# DETERMINISTIC gate verdict: a table over the verdict the graph already
# computed from audit_gate / align_gate / validate_gate. Never a judgment
# made here, and never a re-read of a model field. An unknown verdict is a
# state we did not anticipate, so it BLOCKS — a gate that fails open is
# worse than no gate.
GATE_BLOCKING = {"clean": 0, "committed": 0, "carried": 0,
                 "hold_security": 1, "hold_unstable": 1, "needs_decision": 1,
                 "hold_lost_alignment": 1, "hold_delivery_permission": 1}
gate_on = bool(gate_enabled)
blocking = GATE_BLOCKING.get(s(verdict))
if blocking is None:
    blocking = 1
# The status description a reviewer reads on the PR. Written per verdict
# because the generic "no blocking findings (≥threshold)" phrasing assumes
# a severity floor, and this gate turns on the audit verdict instead.
gate_note = {
    "clean": "supply-chain audit clean; nothing left to align, build verified",
    "committed": "supply-chain audit clean; alignment committed, build verified",
    "hold_security": "supply-chain risk: the bump was not applied",
    "hold_unstable": "build/tests not green after alignment",
    "hold_lost_alignment": "the code alignment for this bump is missing from the branch",
    "hold_delivery_permission": "the alignment edits a GitHub workflow file and the delivery credential was refused the workflows permission",
    "needs_decision": "an architectural decision is pending a human",
    "carried": "supply-chain audit carried from " + carried_from + ": identical change; this base is built by the repo's CI",
}.get(s(verdict), "")
if s(verdict) not in GATE_BLOCKING:
    gate_note = "unrecognised verdict " + (s(verdict) or "(empty)") + " - the gate fails closed"

# The server's publish endpoint is the ONLY path that can post a status:
# the run's own forge_token is minted without the statuses permission.
if s(pub_url) and s(pub_token):
    # The server injects the FULL endpoint URL, not a base — use it
    # verbatim. Appending a path produces a URL that misses the
    # auth-exempt route and is answered by the global auth middleware.
    endpoint = s(pub_url)
    payload = {"pr_url": s(pr_url), "summary": body, "mode": "summary", "comments": []}
    if gate_on:
        payload["gate"] = {"enabled": True, "context": s(gate_context) or "vetty/deps",
                           "blocking_count": blocking, "threshold": "verdict"}
        # Pin the verdict to the revision it is ABOUT. The endpoint resolves
        # the PR head at the moment it posts; without this, a push landing
        # between the audit and the publish takes this verdict as its own.
        # The server refuses a stale pin rather than retargeting the status.
        #
        # The pin is SHAPE-CHECKED here because on the committed path it is
        # read off an agent's report, and this file already records the
        # model answering with the abbreviated form `git commit` prints
        # (see same_commit below). The server REFUSES a pin it cannot read,
        # and that refusal costs the required check - so a malformed answer
        # degrades to an unpinned post, which the server reports as such,
        # rather than to no status at all.
        pin = s(audited_sha)
        if re.match(r"^[0-9a-fA-F]{7,40}$", pin):
            payload["gate"]["audited_sha"] = pin
        elif pin:
            gate_note = (gate_note + " " if gate_note else "") + \
                        "(posted UNPINNED: this run reported " + repr(pin)[:48] + \
                        " as the audited commit, which is not a commit id)"
        # The fact travels with the gate. The note ends with the change
        # identity of the pinned revision (the server refuses a stale
        # pin, so a status on X is a verdict about X), and the review's
        # LAST line names the sha, the id, the verdict and the original
        # audit — what continuity reads, and nothing else. Every note plus
        # the stamp fits GitHub's 140 runes. A carried verdict that cannot
        # name its original audit is published unstamped: stamping it as
        # its own origin would restart the clock its age is measured on.
        pinned = s(payload["gate"].get("audited_sha")).lower() == pin_sha
        if change_id and pinned and (s(verdict) != "carried" or (origin and origin_when)):
            gate_note = gate_note + " · " + change_id
            payload["summary"] = body + "\n\n<!-- vetty-gate/v1 sha=" + pin_sha + " id=" + change_id + \
                " verdict=" + (s(verdict) if s(verdict) in GATE_BLOCKING else "unknown") + \
                " origin=" + (origin or pin_sha) + \
                " origin_at=" + (origin_when or time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())) + " -->"
        if gate_note:
            payload["gate"]["note"] = gate_note
    req = urllib.request.Request(
        endpoint, data=json.dumps(payload).encode("utf-8"), method="POST",
        headers={"Content-Type": "application/json", "X-Iterion-Run": s(pub_token),
                 "User-Agent": "vetty-dep-update-guard"})
    try:
        # A redirect would turn this POST into a GET (urllib's default),
        # and the endpoint's route is POST-only — the request would then
        # miss it and be answered by the auth middleware. Refuse to follow
        # instead of silently degrading into an unexplainable 401.
        class _NoRedirect(urllib.request.HTTPRedirectHandler):
            def redirect_request(self, req, fp, code, msg, headers, newurl):
                raise urllib.error.HTTPError(
                    req.full_url, code,
                    "publish endpoint redirected to " + str(newurl) +
                    " (a redirect turns the POST into a GET and misses the route)",
                    headers, fp)

        opener = urllib.request.build_opener(_NoRedirect)
        with opener.open(req, timeout=120) as resp:
            res = json.loads(resp.read().decode("utf-8"))
        out(bool(res.get("published")),
            comment_url=str(res.get("review_url") or ""),
            skipped_reason=str(res.get("skipped_reason") or ""),
            summary="verdict=" + s(verdict) + "; published via the iterion forge-publish endpoint" +
                    "".join("; " + n for n in (id_note, repin_note) if n),
            gate_posted=bool(res.get("gate_posted")),
            gate_state=str(res.get("gate_state") or ""),
            gate_skipped_reason=str(res.get("gate_error") or ""),
            gate_sha=str(res.get("gate_sha") or ""))
    except urllib.error.HTTPError as e:
        detail = ""
        try: detail = e.read().decode("utf-8")[:300]
        except Exception: pass
        final = ""
        try: final = e.geturl()
        except Exception: pass
        # Name the URL actually called and the one the server answered
        # from: a 401 that says nothing else is indistinguishable between
        # a bad token, a missed route and a redirect, and that ambiguity
        # already cost two debugging cycles.
        gate_fallback = "publish endpoint HTTP %s at %s%s: %s" % (
            e.code, endpoint,
            (" (answered from " + final + ")") if final and final != endpoint else "",
            detail or e.reason)
        # Fall through to the direct comment: the verdict still has to
        # reach the PR. The gate cannot follow it there.
    except Exception as e:
        gate_fallback = "publish endpoint error: " + str(e)
else:
    gate_fallback = "no forge publish grant (local run) - a run token cannot post a commit status"

token = ""
tref = s(token_ref)
if tref:
    token = (open(tref).read().strip() if os.path.exists(tref) else tref)
if not token:
    out(False, skipped_reason="no forge_token bound — cannot post", gate_skipped_reason=gate_fallback)

m = re.match(r"https?://([^/]+)/([^/]+)/([^/]+)/(?:pull|pulls|-/merge_requests|merge_requests)/(\d+)", s(pr_url))
if not m:
    out(False, skipped_reason="unparseable pr_url: " + s(pr_url), gate_skipped_reason=gate_fallback)
host, owner, repo, num = m.group(1), m.group(2), m.group(3), m.group(4)
if host == "github.com":
    api = "https://api.github.com/repos/%s/%s/issues/%s/comments" % (owner, repo, num)
    headers = {"Authorization": "Bearer " + token, "Accept": "application/vnd.github+json"}
else:
    api = "https://%s/api/v1/repos/%s/%s/issues/%s/comments" % (host, owner, repo, num)
    headers = {"Authorization": "token " + token, "Accept": "application/json"}
headers.update({"Content-Type": "application/json", "User-Agent": "vetty-dep-update-guard"})
req = urllib.request.Request(api, data=json.dumps({"body": body}).encode("utf-8"), method="POST", headers=headers)
try:
    with urllib.request.urlopen(req, timeout=30) as resp:
        created = json.loads(resp.read().decode("utf-8"))
    out(True, comment_url=(created.get("html_url") or created.get("url") or ""),
        summary="verdict=" + s(verdict) + "; comment posted",
        gate_skipped_reason=gate_fallback)
except urllib.error.HTTPError as e:
    detail = ""
    try: detail = e.read().decode("utf-8")[:300]
    except Exception: pass
    out(False, skipped_reason="forge POST HTTP %s: %s" % (e.code, detail), gate_skipped_reason=gate_fallback)
except Exception as e:
    out(False, skipped_reason="forge POST error: " + str(e), gate_skipped_reason=gate_fallback)

