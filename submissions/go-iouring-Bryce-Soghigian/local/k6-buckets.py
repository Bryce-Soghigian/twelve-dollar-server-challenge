#!/usr/bin/env python3
"""Per-10s latency from a k6 CSV (K6_OUT): tells server slowness (wait) from k6 starvation (send)."""
import collections, csv, gzip, sys

def p(xs, q):
    xs = sorted(xs)
    return xs[min(len(xs) - 1, int(q * len(xs)))] if xs else 0

b = collections.defaultdict(lambda: collections.defaultdict(list))
t0 = None
with gzip.open(sys.argv[1], 'rt') as f:
    r = csv.reader(f)
    h = next(r)
    mi, ti, vi = h.index('metric_name'), h.index('timestamp'), h.index('metric_value')
    rows = [(row[mi], int(float(row[ti])), float(row[vi])) for row in r
            if row[mi] in ('http_req_duration', 'http_req_waiting', 'http_req_sending', 'vus')]
t0 = min(t for _, t, _ in rows)
for m, t, v in rows:
    b[(t - t0) // 10 * 10][m].append(v)
print(f"{'t(s)':>5} {'vus':>6} {'reqs':>6} {'dur p99':>9} {'wait p99':>9} {'send p99':>9} {'dur max':>8}")
for k in sorted(b):
    d = b[k]
    print(f"{k:>5} {int(max(d['vus'] or [0])):>6} {len(d['http_req_duration']):>6} "
          f"{p(d['http_req_duration'], .99):>7.0f}ms {p(d['http_req_waiting'], .99):>7.0f}ms "
          f"{p(d['http_req_sending'], .99):>7.0f}ms {max(d['http_req_duration'] or [0]):>6.0f}ms")
