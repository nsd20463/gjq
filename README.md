# gjq
simple jq-like program with performance rather than completeness as the goal

It only implements what I need, which at the moment is simply extracting a field from a series of messages.
For example:

    $ echo '[ {"x":1}, {"x":2}, {"x":3} ]' | gjq '[].x'
    1
    2
    3

At the moment gjq is about 4x faster than jq for the sort of input data and filter I'm interested in using it on:

    $ time gjq '.X[].Y' <100k_json_records.json >/dev/null
    real    0m14.131s
    user    0m13.540s
    sys     0m0.500s
    $ time jq '.X[].Y' <100k_json_records.json >/dev/null
    real    1m3.752s
    user    1m2.780s
    sys     0m0.780s
    $ ls -l 100k_json_records.json
    -rw-rw-r-- 1 ndade ndade 2719975876 Jun  2 07:52 100k_json_records.json

The test input is 100,000 lines, each line is a single compacted JSON object. The average size of an object
in this file is 27kB, and I'm extracting a small (20 byte) field from a handful of list elements in each object.
The output contains 171k elements.
