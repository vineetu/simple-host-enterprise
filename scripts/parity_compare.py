#!/usr/bin/env python3
"""Compare parity docs, allowing the owner's explicit Host-only storage design."""
from pathlib import Path
import sys

AREA = "| Site storage primitives (2026-10-02) |"
SCOPE = "hosted / small box only, not Enterprise"


def equivalent(hosted, enterprise):
    # Only this feature row may differ while the two branches are being merged.
    # Shared features, section indexes, security notes and other drift still fail.
    intentional = any(line.startswith(AREA) and SCOPE in line
                      and "`different on purpose`" in line
                      for doc in (hosted, enterprise) for line in doc.splitlines())
    def comparable(doc):
        return "\n".join(line for line in doc.splitlines()
                         if not (intentional and line.startswith(AREA)))
    return comparable(hosted) == comparable(enterprise)


def self_test():
    row = AREA + " orders | excluded | `different on purpose` — " + SCOPE + " |"
    legacy = AREA + " old primitives | excluded | `different on purpose` |"
    shared = "| Shared | yes | yes | `same` |"
    assert equivalent(row + "\n" + shared, legacy + "\n" + shared)
    assert equivalent(row + "\n" + shared, shared)
    assert not equivalent(row + "\n" + shared, legacy + "\n" + shared.replace("yes", "no"))
    assert not equivalent(legacy, legacy.replace("old primitives", "unmarked drift"))
    assert not equivalent(row, row + "\n| new gap | no | yes | `gap → hosted` |")
    print("check-parity: intentional storage difference passes; unrelated drift still fails")


if __name__ == "__main__":
    if sys.argv[1:] == ["--self-test"]:
        self_test()
    elif len(sys.argv) == 3:
        sys.exit(0 if equivalent(*(Path(p).read_text() for p in sys.argv[1:])) else 1)
    else:
        sys.exit("usage: parity_compare.py HOSTED_PARITY ENTERPRISE_PARITY | --self-test")
