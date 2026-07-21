# Shared library for reddit-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were builtins — without Starlark's load() (which stunt does not
# support). See internal/starlark/vm.go LoadWithLib.

# _has_ua checks whether the request has a descriptive User-Agent.
# Reddit bans absent/generic UAs. Accept only a descriptive one (our
# adapter sends "myapp/1.0 (...)"). This is what makes the
# missing-UA bug reproducible.
def _has_ua(req):
    ua = req["headers"].get("User-Agent", "")
    return ua != "" and ua.find("/") >= 0 and ua.find("(") >= 0

# _ua_rejected returns the standard 429 response for a missing/generic UA.
def _ua_rejected(req):
    return respond(429, {"message": "Too Many Requests", "error": 429})
