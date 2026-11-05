<!-- Original text: MIT (LICENSE). Design ideas: https://claude.dev/blog/automating-eval-design-and-hillclimbing/ (source retains its own rights). See THIRD_PARTY_NOTICES.md. -->
# Improve a requested behavior

Run this procedure only when the owner requests a behavior experiment.
Use your own Bash, git worktree, and subagent tools.
Keep the experiment within the owner's requested scope.

1. Delegate representative cases and checkable rubric claims to a separate subagent. Include successes and failures.
2. Have that subagent randomly split cases into training and held-out sets. Read only the training subset.
3. Use temporary worktrees and fresh sessions. Replace side effects with local recording stubs in your experiment runner.
4. Run the unchanged baseline at least twice. Check grading consistency and measure score variation.
5. Diagnose training failures and make one change that fixes a root cause. Do not paste failed transcripts into instructions.
6. Ask the subagent to evaluate baseline and candidate on both splits. Request held-out aggregates and regression counts only.
7. Keep a change only when held-out improvement exceeds noise and no previously passing case breaks. Revert flat or regressing results.
8. Stop after two or three rounds without improvement. Classify remaining failures and report limitations.
9. Remove temporary worktrees and cases. Report measurements, adopted changes, and reverts to the owner with `reply`.
