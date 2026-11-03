import json, os, re, urllib.request, urllib.error
null = None; true = True; false = False  # JSON-literal shim for missing refs
enabled = true
respect_human_arming = true
method = "squash"
pr_url = "https://github.com/SocialGouv/buildkit-operator/pull/38"
verdict = "clean"
gate_posted = true
gate_state = "success"
gate_sha = "9e1fb0a0271de277096e7bed36cf12acba13abe3"
audited_sha = "9e1fb0a0271de277096e7bed36cf12acba13abe3"
committed_sha = ""
gate_on = true
token_ref = "/run/iterion/secrets/forge_token"

def s(x):
    return (x or "").strip() if isinstance(x, str) else ("" if x is None else str(x).strip())

# Set once a prior arming has been taken down. Every exit runs through
# out(), so the note reaches the reason whatever path refuses afterwards -
# a PR left bare must never be silent about it.
state_note = {"disarmed": "", "dequeued": False, "stuck": "", "preserved": ""}

def out(armed, reason=""):
    reason = reason or ""
    notes = []
    if state_note["disarmed"]:
        notes.append("the prior auto-merge arming by " + state_note["disarmed"] +
                     " was disarmed" + ("" if armed else " and NOT replaced"))
    if state_note["preserved"]:
        notes.append(state_note["preserved"])
    if not bool(armed):
        if state_note["dequeued"]:
            notes.append("the pull request was removed from the merge queue")
        # A commitment this run could NOT take down is the one the reader
        # most needs to hear about: the PR stays on course to merge.
        if state_note["stuck"]:
            notes.append("STILL STANDING: " + state_note["stuck"])
    if notes:
        reason = (reason + " | " if reason else "") + "; ".join(notes)
    if not bool(armed) and (state_note["disarmed"] or state_note["dequeued"]):
        reason += " - re-arm by hand if it was wanted"
    print(json.dumps({"armed": bool(armed), "reason": reason}))
    raise SystemExit(0)

# Off means no mandate. Not "undo what somebody else decided": the forge is
# not touched at all, so an operator's own arming survives the switch.
if not bool(enabled):
    out(False, "arm_automerge is off (default)")

# The verdict and gate checks below no longer exit on the spot. A refusal
# has to DISARM what an earlier pass under our identity armed: a hold says
# this revision must not merge, and an arming outlives the run that placed
# it - the forge goes on waiting for the checks either way. So the reason
# is computed here and acted on once the forge is reachable.
refusal = ""
if s(verdict) not in ("clean", "committed"):
    refusal = "verdict " + (s(verdict) or "(empty)") + " is not green"
# The gate is what a repo makes required. Arming while it is missing would
# merge on the strength of a verdict the PR never displayed.
elif bool(gate_on) and not bool(gate_posted):
    refusal = "the merge gate did not land - not arming on a verdict the PR cannot show"
elif bool(gate_on) and s(gate_state) and s(gate_state) != "success":
    refusal = "merge gate is " + s(gate_state)

# Where the forge cannot be reached at all there is nothing to disarm, and
# the refusal - when there is one - is the more useful of the two reasons:
# it is about the bump, not about the plumbing. But a refusal the forge
# never hears about leaves any live commitment standing with nobody able
# to take it down, and a PR left in a state the verdict contradicts must
# SAY so - the same clause the failed read/write exit below carries.
def unreachable(why):
    if refusal:
        out(False, refusal + " | a commitment to merge may still stand: "
                   "the forge was unreachable (" + why + ")")
    out(False, why)

m = re.match(r"https?://([^/]+)/([^/]+)/([^/]+)/(?:pull|pulls)/(\d+)", s(pr_url))
if not m:
    unreachable("auto-merge arming is GitHub-only; no-op for " + (s(pr_url) or "(no pr_url)"))
host, owner, repo, num = m.group(1), m.group(2), m.group(3), int(m.group(4))
if host != "github.com":
    # A GHES host is the case that bites: an arming CAN stand there, and
    # this bot only speaks to api.github.com.
    unreachable("auto-merge arming is GitHub-only; no-op for " + host)

token = ""
tref = s(token_ref)
if tref:
    token = (open(tref).read().strip() if os.path.exists(tref) else tref)
if not token:
    unreachable("no forge_token bound")

def graphql(query, variables):
    req = urllib.request.Request(
        "https://api.github.com/graphql",
        data=json.dumps({"query": query, "variables": variables}).encode("utf-8"),
        method="POST",
        headers={"Authorization": "Bearer " + token, "Content-Type": "application/json",
                 # mergeStateStatus is behind this media type; without it the
                 # field is rejected and the whole query fails.
                 "Accept": "application/vnd.github.merge-info-preview+json",
                 "User-Agent": "vetty-dep-update-guard"})
    with urllib.request.urlopen(req, timeout=30) as resp:
        payload = json.loads(resp.read().decode("utf-8"))
    if payload.get("errors"):
        # Messages first, then the raw structure: a caller distinguishes
        # "nothing left to wait for" from a real failure by reading the
        # message, and truncating the whole structure can cut it off.
        errs = payload["errors"]
        msgs = "; ".join(str(e.get("message", "")) for e in errs if isinstance(e, dict))
        raise RuntimeError((msgs or "") + " | " + str(errs)[:300])
    return payload.get("data") or {}

# The variable sigil is written as @ and swapped at runtime: a doubled
# brace anywhere in this file is a template reference, and a GraphQL block
# that produced one would be substituted away before python ever saw it.
# The placeholder must NOT be a character GraphQL already uses — a colon
# separates every field and argument, so swapping colons corrupts the
# whole query into something the API rejects.
at = chr(36)
q = ("query(@owner: String!, @name: String!, @number: Int!) { "
     "viewer { login } "
     "repository(owner: @owner, name: @name) { "
     "pullRequest(number: @number) { id headRefOid baseRefName mergeable mergeStateStatus "
     "autoMergeRequest { enabledAt enabledBy { login } } mergeQueueEntry { id } } "
     "} }").replace("@", at)
# A non-null mergeQueue on the PR's base branch is what makes this a
# queue-protected repo: direct merges are rejected there, enqueueing is
# the only door, and the forge is the authority on which shape this is.
queue_q = ("query(@owner: String!, @name: String!, @branch: String!) { "
           "repository(owner: @owner, name: @name) { "
           "mergeQueue(branch: @branch) { id } "
           "} }").replace("@", at)
# The ARMING carries the pin for the window this run can close: between the
# head read above and this mutation landing, a push can move the branch,
# and a PINNED enable is REFUSED there instead of arming a commit nobody
# audited (the behaviour witness is the forge that enforces the pin in
# bots/dep_update_guard_arming_pin_test.go, which asserts nothing is armed,
# merged or enqueued on the commit that arrived in the window).
# What the pin does NOT establish is the EVENTUAL merge:
# GitHub documents expectedHeadOid only as "the expected head OID of the
# pull request", and AutoMergeRequest exposes enabledAt, enabledBy,
# mergeMethod and the commit message - never expectedHeadOid - so a
# deferred pin has no visible place to live. An arming OUTLIVES this run,
# and a push landing afterwards moves the head under it - GitHub cancels an
# arming by itself only when the pusher LACKS write, which a dependency
# bot's App does not. What covers THAT push is the required gate re-landing
# on each new head (review_on_sync), not this pin; the gestures that ARE
# atomic are the pinned mergePullRequest and enqueuePullRequest below. The
# experiment that would settle the eventual-merge reading either way is
# filed on #1637, not assumed here - the manifest says the same.
arm_mut = ("mutation(@id: ID!, @method: PullRequestMergeMethod!, @oid: GitObjectID!) { "
           "enablePullRequestAutoMerge(input: { pullRequestId: @id, mergeMethod: @method, "
           "expectedHeadOid: @oid }) { clientMutationId } }").replace("@", at)
# An arming already on the PR cannot be inspected: AutoMergeRequest exposes
# enabledAt, enabledBy, mergeMethod and the commit message - and NOT
# expectedHeadOid. "Already armed" therefore says nothing about which commit
# the forge will merge. The only arming whose pin is known is the one this
# run places. Replace only an arming we own, unless the operator explicitly
# opts into taking over another actor's decision (respect_human_arming=false).
disarm_mut = ("mutation(@id: ID!) { "
              "disablePullRequestAutoMerge(input: { pullRequestId: @id }) "
              "{ clientMutationId } }").replace("@", at)
# A queue ENTRY outlives the run exactly as an arming does, and is strictly
# more committed: once the forge moves an armed PR into the queue,
# autoMergeRequest is null and a disarm is a no-op while the queue still
# merges the bump. The input field is `id` (the pull request), NOT
# `pullRequestId` - the API rejects the latter.
dequeue_mut = ("mutation(@id: ID!) { "
               "dequeuePullRequest(input: { id: @id }) "
               "{ clientMutationId } }").replace("@", at)
# expectedHeadOid is the safety pin: the forge merges this exact commit or
# nothing, so a push racing the decision can never be merged unreviewed.
merge_mut = ("mutation(@id: ID!, @method: PullRequestMergeMethod!, @oid: GitObjectID!) { "
             "mergePullRequest(input: { pullRequestId: @id, mergeMethod: @method, "
             "expectedHeadOid: @oid }) { clientMutationId } }").replace("@", at)
# Same pin on the queue door: enqueue this exact commit or nothing.
enqueue_mut = ("mutation(@id: ID!, @oid: GitObjectID!) { "
               "enqueuePullRequest(input: { pullRequestId: @id, expectedHeadOid: @oid }) "
               "{ clientMutationId } }").replace("@", at)

bot_login = ""

def read_pr():
    global bot_login
    data = graphql(q, {"owner": owner, "name": repo, "number": num})
    bot_login = s((data.get("viewer") or {}).get("login"))
    return ((data.get("repository") or {}).get("pullRequest") or {})

def base_has_merge_queue(pr):
    branch = s(pr.get("baseRefName"))
    if not branch:
        return False
    try:
        data = graphql(queue_q, {"owner": owner, "name": repo, "branch": branch})
        return bool((data.get("repository") or {}).get("mergeQueue"))
    except Exception:
        # An older GHES without the field, or a transient read failure:
        # treat as no queue and let the no-queue path speak. Its own
        # errors are reported, never swallowed.
        return False

# A merge may only ever target the commit THIS run audited and gated.
# Re-reading the head does not give that commit: the audit takes minutes,
# and a rebase or force-push in between produces a head no auditor has
# seen. Nor does the forge hold such a head back — a commit status only
# blocks a merge once a maintainer has made its context required — so CLEAN
# is perfectly reachable on unreviewed code.
# The shas being compared come from three sources with different widths:
# the forge always returns full 40-hex, `git rev-parse HEAD` does too, but
# committed_sha is reported by the commit AGENT, and the natural thing to
# read off `git commit` is the abbreviated form it prints. An exact compare
# therefore refused the merge on the align-and-commit path — Vetty's
# primary path — with a message ("something else pushed") that was simply
# false. Compare on the common prefix, with git's own 7-hex floor so a
# truncation that short cannot make two different commits look equal.
def same_commit(a, b):
    a, b = s(a).lower(), s(b).lower()
    if not a or not b:
        return False
    n = min(len(a), len(b))
    if n < 7:
        return False
    return a[:n] == b[:n]

def vouched_sha(pr, act):
    # The commit any merge or enqueue may target: the one THIS run
    # audited, gated, and can still see at the branch head.
    head = s(pr.get("headRefOid"))
    gated = s(gate_sha)
    # What THIS run read, and what it pushed if it aligned anything.
    mine = s(committed_sha) or s(audited_sha)
    if not gated:
        out(False, "no gated sha to " + act + " against - acting on the current head "
                   "would ship a commit this run never audited")
    if not mine:
        out(False, "this run cannot name the commit it audited - refusing to " + act +
                   " on a sha it cannot vouch for")
    # gate_sha comes from the FORGE (the head at publish time), not from
    # this run: a push landing between the alignment and the status read
    # would carry the gate onto a commit no auditor ever saw. It is only
    # trustworthy where it agrees with what this run produced.
    # The two exits ABOVE are structural - the gate never ran, or this run
    # cannot name what it read - so the current head may well still be the
    # audited one and nothing is taken down. The two BELOW are the opposite:
    # the head is demonstrably NOT what was audited, so any commitment
    # standing on this PR would land an unaudited revision. Those stand
    # down within the ownership policy (a human decision is preserved).
    if not same_commit(gated, mine):
        stand_down(pr)
        out(False, "the gate landed on " + gated[:12] + " but this run audited " +
                   mine[:12] + " - the branch head and the audited commit disagree")
    if head and not same_commit(head, gated):
        stand_down(pr)
        out(False, "the branch moved after the audit (gated " + gated[:12] +
                   ", head is now " + head[:12] + ") - the new head has not been audited")
    return gated

def merge_now(pr, why):
    gated = vouched_sha(pr, "merge")
    # A re-read after a refused enable may observe a maintainer's NEW
    # arming. Mutation W3/re-read: drop this check -> merge their decision.
    if pr.get("autoMergeRequest") and preserve_arming(pr):
        out(True)
    graphql(merge_mut, {"id": pr.get("id"), "method": s(method).upper() or "SQUASH",
                        "oid": gated})
    out(True, superseding_reason(pr, why))

def enqueue_now(pr, why):
    gated = vouched_sha(pr, "enqueue")
    # Same ownership check on the queue fallback, after vouching the head.
    # Mutation W3/re-read: drop this check -> enqueue another actor's arming.
    if pr.get("autoMergeRequest") and preserve_arming(pr):
        out(True)
    graphql(enqueue_mut, {"id": pr.get("id"), "oid": gated})
    out(True, superseding_reason(pr, why))

# Ready to merge means every required check the forge knows about is
# already satisfied — the state auto-merge exists to wait for.
def ready(pr):
    return s(pr.get("mergeStateStatus")).upper() == "CLEAN" and \
           s(pr.get("mergeable")).upper() == "MERGEABLE"

def arming_owner(pr):
    return s(((pr.get("autoMergeRequest") or {}).get("enabledBy") or {}).get("login"))

def superseding_reason(pr, why):
    # A successful merge/enqueue can consume an arming observed on the
    # fallback read. Name it even when no explicit disarm was needed.
    if pr.get("autoMergeRequest"):
        return why + " | superseded auto-merge arming by " + (arming_owner(pr) or "(unknown owner)")
    return why

def preserve_arming(pr):
    # A missing launch var renders null: keep the historical policy for
    # those old inputs. New launches receive the declared true default.
    # Mutation W4: ignore this opt-out -> legacy takedowns disappear.
    if not bool(respect_human_arming):
        return False
    login = arming_owner(pr)
    # Mutation W2: remove this ownership test -> the bot cannot stand down
    # its own arming. Missing identity is NOT proof of ownership.
    if login and bot_login and login.casefold() == bot_login.casefold():
        return False
    state_note["preserved"] = ("auto-merge arming by " + (login or "(unknown owner)") +
                               " preserved: respect_human_arming=true; "
                               "this decision is not owned by this bot")
    return True

def disarm(pr):
    # Records the takedown BEFORE claiming it: out() appends the note to
    # every later refusal, and a disarm that half-succeeded must not read
    # as one that never happened.
    login = arming_owner(pr) or "(unknown owner)"
    try:
        graphql(disarm_mut, {"id": pr.get("id")})
    except Exception as e:
        state_note["stuck"] = "the auto-merge arming by " + login + \
                              " could not be removed (" + str(e)[:90] + ")"
        raise
    state_note["disarmed"] = login

def stand_down(pr):
    # ONE place that stands down commitments within our mandate:
    # the owned arming AND the queue entry. Two, because the forge nulls
    # autoMergeRequest once it moves an armed PR into the queue - taking
    # only the arming down there is a no-op while the queue still merges.
    # Best-effort and never masks the refusal it precedes: what could not
    # be removed is named in the reason instead.
    node_id = pr.get("id")
    if not node_id:
        return
    # Mutation W1: drop `and not preserve_arming(pr)` -> human arming dies.
    if pr.get("autoMergeRequest") and not preserve_arming(pr):
        try:
            disarm(pr)
        except Exception:
            pass  # disarm recorded the owner and the failed takedown
    if pr.get("mergeQueueEntry"):
        try:
            graphql(dequeue_mut, {"id": node_id})
            state_note["dequeued"] = True
        except Exception as e:
            state_note["stuck"] = "the queue entry could not be removed (" + str(e)[:90] + ")"

# A refusal that has to take an arming down: read the PR, disarm if one is
# there, then report. A forge failure here never masks the refusal - the
# bump's verdict is the thing the PR has to carry away.
if refusal:
    try:
        stand_down(read_pr())
    except Exception as e:
        out(False, refusal + " | a commitment to merge may still stand: the forge could not be "
                   "read or written (" + str(e)[:110] + ")")
    out(False, refusal)

try:
    pr = read_pr()
    node_id = pr.get("id")
    if not node_id:
        out(False, "pull request not found via the forge API")
    # Vouching comes FIRST, before any state on the PR is acknowledged as
    # good. An existing queue entry is as opaque as an existing arming -
    # nothing says WHICH commit the forge queued - so acknowledging it
    # before checking the head would bless a queued revision this run never
    # audited, exactly the hole "already armed" used to be.
    # The pin is resolved BEFORE anything is taken down. vouched_sha exits
    # on four distinct disagreements, and a disarm placed ahead of it leaves
    # the pull request BARE on a refusal this run could have made without
    # touching the forge at all - worst with gate_enabled off, where no
    # gate_sha is ever produced, so EVERY pass would tear an arming down and
    # then refuse to replace it.
    pin = vouched_sha(pr, "arm")
    if pr.get("mergeQueueEntry"):
        out(True, "already in the merge queue")
    # An arming already on the PR is opaque - its expectedHeadOid cannot be
    # read back - so an owned one is replaced by one this run can vouch
    # for. Another actor's decision survives when respect_human_arming is
    # on; reporting it as standing does not certify its pin. Disarm first:
    # enablePullRequestAutoMerge on an armed PR is refused, and re-arming
    # is the whole point.
    if pr.get("autoMergeRequest"):
        # Mutation W3: drop this early exit -> a human's arming is replaced.
        if preserve_arming(pr):
            out(True)
        disarm(pr)
    # Arm FIRST, whatever the state. On a queue repo this is "merge when
    # ready" and covers CLEAN too; on a plain repo a CLEAN PR is refused
    # ("clean status") and falls through to the pinned direct merge. The
    # ordering matters: a merge-now-first bot direct-merges into a queue's
    # rejection and never lands anything on a queue-protected repo.
    try:
        graphql(arm_mut, {"id": node_id, "method": s(method).upper() or "SQUASH",
                          "oid": pin})
    except Exception as arm_err:
        if base_has_merge_queue(pr):
            # The queue is the only door here; a direct merge would be
            # rejected outright. Enqueue the audited commit or nothing.
            pr = read_pr()
            if pr.get("mergeQueueEntry"):
                out(True, "already in the merge queue")
            enqueue_now(pr, "enqueued: the merge queue decides from here "
                            "(arming was refused: " + str(arm_err)[:120] + ")")
        # No queue: the one refusal that means "nothing left to wait for"
        # is the CLEAN state. The state can flip between the read above
        # and this call, so re-read rather than trusting the first answer.
        if "clean status" not in str(arm_err).lower():
            raise
        pr = read_pr()
        if not ready(pr):
            out(False, "auto-merge request refused: " + str(arm_err))
        merge_now(pr, "merged: the forge reported every required check already green")
    out(True, "")
except urllib.error.HTTPError as e:
    detail = ""
    try: detail = e.read().decode("utf-8")[:300]
    except Exception: pass
    out(False, "forge HTTP %s: %s" % (e.code, detail))
except Exception as e:
    # The usual cause is auto-merge disabled on the repo, or a merge method
    # the repo does not allow. Reported, never swallowed: an un-merged PR
    # must always carry its reason.
    out(False, "auto-merge request refused: " + str(e))

