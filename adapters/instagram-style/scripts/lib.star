# Shared library for instagram-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were builtins — without Starlark's load() (which stunt does not
# support). See internal/starlark/vm.go LoadWithLib.

# _bearer_present checks whether an Authorization: Bearer header is present.
# The token value is NOT validated (token-PRESENCE policy for Instagram).
def _bearer_present(req):
    auth = req["headers"].get("Authorization", "")
    if auth[:7] == "Bearer ":
        return True
    return False
