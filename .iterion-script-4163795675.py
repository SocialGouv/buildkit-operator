import json, os, re, urllib.request, urllib.error
null = None; true = True; false = False  # JSON-literal shim for missing refs
pr_url = "https://github.com/SocialGouv/buildkit-operator/pull/38"
verdict = "clean"
audit_summary = "Renovate digest bump of the gcr.io/distroless/static:nonroot base image (sha256:f7f8f72… → sha256:25ac8f0c…) across the three runtime Dockerfiles (cmd/buildd, cmd/companion, cmd/gateway). Scope verified against the branch: git diff main...HEAD touches exactly those 3 files, one line each — an isolated digest pin, matching the PR title; diff is the true base..HEAD range (degraded=false), and the changed files ARE the manifests (Dockerfiles). Registry audit: the live tag gcr.io/distroless/static:nonroot resolves today to exactly the NEW digest, so the pin matches the tag Renovate saw — no re-pointing/hijack signal; both old and new digests resolve as valid multi-arch OCI indexes with an identical 6-platform matrix (amd64, arm/v7, arm64/v8, ppc64le, riscv64, s390x). Image-config diff: User=65532 (nonroot) preserved, no Entrypoint/Cmd, Env identical (PATH + SSL_CERT_FILE), 13 history layers in both; the only non-layer change is upstream's bazel target rename cacerts_debian13_amd64 → cacerts_debian13_amd64_tar (CA-cert packaging change in the distroless build) — no new build-time fetch steps, no obfuscated content. The content delta is a routine freshness rebuild: Debian 13.6 → 13.7 point release (base-files 13.8+deb13u6 → u7), tzdata 2026b → 2026f, and ca-certificates now shipped as a proper dpkg package (20250419+deb13u1) instead of a raw tar layer. CVE delta is clean on both sides: trivy found 0 vulnerabilities in the old image AND the new image (verified the scan inspected the real dpkg database — debian 13.7, 6 packages — not a hollow result), and direct OSV API queries for every package in both images returned zero advisories. No malware signals: image built by Google's reproducible (epoch-timestamped) distroless bazel pipeline at the canonical gcr.io/distroless registry; a container image has no install lifecycle hooks to abuse, and the old pin was not flagged. Nothing for this bump to resolve, nothing introduced, no consuming code to align (Dockerfiles reference the new digest directly; nothing else in the repo references the old one). Caveat for the record: osv-scanner in this sandbox cannot scan images directly, so the OSV layer ran as per-package API queries instead; trivy's advisory DB is as of today (2026-11-03). Routine, safe bump — no human review needed."
cves = null
malware_signals = null
align_summary = "gcr.io/distroless/static:nonroot digest f7f8f72 → 25ac8f0: a base-image rebuild with no API surface, so no consuming code needed alignment. The only consumers are the three Dockerfiles' FROM lines (cmd/buildd, cmd/companion, cmd/gateway), which the bot already updated consistently to the new digest; a repo-wide grep found no other reference to the image and no stale old-digest pin (k8s manifests, helm chart, docs, workflows — none touch it). Supply-chain audit: the new digest resolves at gcr.io with an exact Docker-Content-Digest match; it is an OCI image index with the identical 6-arch set as the old digest (linux/amd64, arm64/v8, arm/v7, s390x, ppc64le, riscv64), so TARGETARCH multi-arch builds are unaffected; cosign verify passes against Google's official distroless build identity (keyless@distroless.iam.gserviceaccount.com, issuer accounts.google.com) with a valid Sigstore cert chain and Rekor transparency-log bundle. No malware/compromise signal, nothing unresolved HIGH/CRITICAL introduced."
validate_summary = "deterministic verify green (exit 0); log tail: NOT COVERED: image cmd/buildd/Dockerfile, cmd/companion/Dockerfile, cmd/gateway/Dockerfile — built by .github/workflows/images.yml (CI only; sandbox has no Docker daemon). This branch's distroless digest bump lands there.\nNOT COVERED: e2e suite test/e2e (build tag \"e2e\", live OVH cluster; never run by PR CI).\ncheck-chart-flags: buildd — 35 rendered flags, all defined\ncheck-chart-flags: gateway — 7 rendered flags, all defined\nok  \tgithub.com/socialgouv/buildkit-operator/internal/projectdefaults\t1.070s\tcoverage: 97.8% of statements\n?   \tgithub.com/socialgouv/buildkit-operator/internal/provisioner\t[no test files]\nok  \tgithub.com/socialgouv/buildkit-operator/internal/provisioner/k8s\t8.217s\tcoverage: 81.2% of statements\nok  \tgithub.com/socialgouv/buildkit-operator/internal/provisioner/local\t8.425s\tcoverage: 60.8% of statements\nok  \tgithub.com/socialgouv/buildkit-operator/internal/router\t1.076s\tcoverage: 92.0% of statements\nunit-testable coverage (excl. generated + bootstrap): 86.7% (threshold 85%)\ntask: [lint] go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.15.0 run ./...\n0 issues.\nverify: all ci.yml gates green\n"
escalation = null
commit_summary = "nothing to commit: the working tree matches the branch head (renovate/gcr.io-distroless-static-nonroot @ fee1635, the bot's digest bump). The alignment pass changed no files (Files aligned: []) — the distroless digest bump is a base-image rebuild with no consuming code to align, and the only references (cmd/buildd, cmd/companion, cmd/gateway Dockerfile FROM lines) were already updated by Renovate in fee1635. No alignment commit made; nothing pushed to origin."
audited_sha = "fee1635e295ea9663f10d7ee2c65c0fae6d5cef3"
token_ref = "/run/iterion/secrets/forge_token"
pub_url = "https://iterion.cloud/api/v1/forge/publish-review"
run_url = "https://iterion.cloud/studio/runs/01a1a2b1-9248-7b65-bb59-a0592b152de3"
run_id = "01a1a2b1-9248-7b65-bb59-a0592b152de3"
# The grant reaches this script through the interpreter's ENVIRONMENT,
# under the engine's placeholder for the var: a script body is written to
# a temp file (and pushed into the workspace by a copy-based sandbox), so
# an inlined bearer would sit in clear on disk for the node's lifetime.
pub_token = os.environ.get("__ITERION_SECRET_forge_publish_token__", "")
gate_enabled = true
gate_context = "revi/review"

def s(x):
    return (x or "").strip() if isinstance(x, str) else ("" if x is None else str(x).strip())

def out(posted, comment_url="", skipped_reason="", summary="",
        gate_posted=False, gate_state="", gate_skipped_reason="", gate_sha=""):
    print(json.dumps({"verdict": s(verdict),
                      "posted": bool(posted), "comment_url": comment_url or "",
                      "skipped_reason": skipped_reason or "", "summary": summary or "",
                      "gate_posted": bool(gate_posted), "gate_state": gate_state or "",
                      "gate_skipped_reason": gate_skipped_reason or "",
                      "gate_sha": gate_sha or ""}))
    raise SystemExit(0)

if not s(pr_url):
    out(False, skipped_reason="no pr_url (local run)")

badges = {
    "hold_security":  "\U0001F6D1 HOLD — supply-chain risk; not aligned/merged",
    "needs_decision": "\U0001F9ED Needs a human decision (architectural)",
    "hold_unstable":  "⚠️ HOLD — build/tests not green after alignment",
    "hold_lost_alignment": "\U0001F6D1 HOLD — the alignment for this bump is NOT on the branch",
    "hold_delivery_permission": "\U0001F6D1 HOLD — this bump's alignment edits a workflow file, and the delivery permission for it was refused",
    "committed":      "✅ Aligned — code updated on this branch; review & merge",
    "clean":          "✅ Safe bump — nothing left to align",
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
       "clean": "Nothing left to align."}.get(s(verdict), "")
did = (s(commit_summary) + " " if s(commit_summary) else "") + did + \
    " **Vetty never merges past a check** — whether this lands is the repo's own required checks' decision, " \
    "and only the exact commit reported above can be merged."
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
body = "\n".join(parts)

# DETERMINISTIC gate verdict: a table over the verdict the graph already
# computed from audit_gate / align_gate / validate_gate. Never a judgment
# made here, and never a re-read of a model field. An unknown verdict is a
# state we did not anticipate, so it BLOCKS — a gate that fails open is
# worse than no gate.
GATE_BLOCKING = {"clean": 0, "committed": 0,
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
            summary="verdict=" + s(verdict) + "; published via the iterion forge-publish endpoint",
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

