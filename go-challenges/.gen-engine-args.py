#!/usr/bin/env python3
"""Emit go-quality-engine args JSON for the given chapter dirs.

Usage: python3 .gen-engine-args.py <chapter-dir> [<chapter-dir> ...]

Mode classification (gate = must build/test offline; bar = offline capstone bar
for cgo / external modules / assembly / profile-dependent lessons). The BAR map
lists, per chapter, the lesson NN- prefixes that cannot gate offline; everything
else defaults to gate. Capstone chapters (38-47) are all bar.
"""
import json, os, sys

# lesson NN prefixes that must be mode "bar" (cannot build/test offline)
BAR = {
    "30-production-patterns": {"07", "08"},                 # opentelemetry
    "31-cloud-native-go": {"01","02","03","04","05","06","07","08","10","11"},  # aws-sdk/client-go/tf/otel/prom; only 09 gates
    "33-tcp-udp-and-networking": {"13","14","24","25","26","27","28"},  # grpc/quic/vpn/stun/bpf
    "36-runtime-compiler-and-assembly": {"05","09","10","11"},  # pgo profile + .s assembly
}

def is_capstone(ch):
    n = ch.split("-")[0]
    return n.isdigit() and 38 <= int(n) <= 47

def lessons(ch):
    base = os.path.join("challenges/go", ch)
    bar = BAR.get(ch, set())
    cap = is_capstone(ch)
    out = []
    for name in sorted(os.listdir(base)):
        p = os.path.join(base, name)
        if not os.path.isdir(p):
            continue
        md = os.path.join(p, name + ".md")
        if not os.path.exists(md):
            continue
        nn = name.split("-")[0]
        mode = "bar" if (cap or nn in bar) else "gate"
        out.append({"path": md, "mode": mode, "generate": True})
    return out

chapters = [{"name": ch, "lessons": lessons(ch)} for ch in sys.argv[1:]]
total = sum(len(c["lessons"]) for c in chapters)
sys.stderr.write(f"chapters={len(chapters)} lessons={total}\n")
print(json.dumps({"chapters": chapters}))
