# gjq
simple jq-like program with performance rather than completeness as the goal

It only implements what I need, which at the moment is simply extracting a field from a series of messages.
For example:

  echo '[ {"x":1}, {"x":2}, {"x":3} ]' | gjq '[].x'
  1
  2
  3
