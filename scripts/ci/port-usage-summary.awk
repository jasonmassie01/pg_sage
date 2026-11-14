# Summarises port-usage.csv (ci-16): peak dynamic ports in use, TIME_WAIT
# and connections to the test servers, per group in order of appearance,
# then the round's peak and whether it came near the dynamic range.
# Run by port-usage.sh summary with -F, -v range="<first> <count>" -v near=<pct>.
NR == 1 { split(range, r, " "); size = r[2] + 0; next }
NF < 7 { next }
{
  g = $2
  if (!(g in samples)) order[++groups] = g
  samples[g]++
  if (samples[g] == 1 || $3 + 0 > dyn[g]) { dyn[g] = $3 + 0; at[g] = $1 }
  if ($4 + 0 > tw[g]) tw[g] = $4 + 0
  if ($6 + 0 > wtw[g]) wtw[g] = $6 + 0
  if ($7 + 0 > west[g]) west[g] = $7 + 0
  if (total == 0 || $3 + 0 > peak) { peak = $3 + 0; peak_at = $1; peak_group = g }
  total++
}
function pct(n) { return size > 0 ? 100 * n / size : 0 }
END {
  if (total == 0) { print "No port-usage samples."; exit 1 }
  print "# Port usage (ci-16)"
  print ""
  printf "Dynamic range: %d ports from %d. Samples: %d.\n\n", size, r[1], total
  print "| Group | Samples | Peak dynamic in use | Share | At | Peak TIME_WAIT |" \
    " Peak TIME_WAIT to servers | Peak ESTABLISHED to servers |"
  print "|---|---|---|---|---|---|---|---|"
  for (i = 1; i <= groups; i++) {
    g = order[i]
    printf "| %s | %d | %d | %.1f%% | %s | %d | %d | %d |\n",
      g, samples[g], dyn[g], pct(dyn[g]), at[g], tw[g], wtw[g], west[g]
  }
  print ""
  printf "Peak: %d of %d dynamic ports (%.1f%%) at %s during group %s.\n",
    peak, size, pct(peak), peak_at, peak_group
  if (pct(peak) >= near)
    printf "Verdict: near the dynamic range (>= %d%%): connection churn can exhaust it.\n", near
  else
    printf "Verdict: far from the dynamic range (< %d%%): look for another cause.\n", near
}
