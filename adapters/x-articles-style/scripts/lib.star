# Shared library for x-articles-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were builtins — without Starlark's load() (which stunt does not
# support). See internal/starlark/vm.go LoadWithLib.

# _pad5 zero-pads a non-negative integer to 5 digits.
def _pad5(n):
    if n < 10:
        return "0000" + str(n)
    if n < 100:
        return "000" + str(n)
    if n < 1000:
        return "00" + str(n)
    if n < 10000:
        return "0" + str(n)
    return str(n)
