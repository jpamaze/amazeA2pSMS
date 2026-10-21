-- KEYS[1] range block flag
-- KEYS[2] the range's hash for this hour: field <last 3 digits> = messages
--         to that number this hour, field s<0-5> = messages into the range
--         in that 10 minutes. One key per range per hour keeps Redis small.
-- ARGV: number field, 10-minute field, per-number limit, per-range limit, block seconds
-- Returns 0 ok, 1 number over its limit, 2 range just blocked, 3 range blocked.
if redis.call('EXISTS', KEYS[1]) == 1 then
  return 3
end
local destLimit, seriesLimit = tonumber(ARGV[3]), tonumber(ARGV[4])
local fresh = redis.call('EXISTS', KEYS[2]) == 0
if destLimit > 0 then
  local d = redis.call('HINCRBY', KEYS[2], ARGV[1], 1)
  if fresh then redis.call('EXPIRE', KEYS[2], 3700) end
  if d > destLimit then return 1 end
end
if seriesLimit > 0 then
  local s = redis.call('HINCRBY', KEYS[2], ARGV[2], 1)
  if fresh then redis.call('EXPIRE', KEYS[2], 3700) end
  if s > seriesLimit then
    if tonumber(ARGV[5]) > 0 then
      redis.call('SET', KEYS[1], 1, 'EX', ARGV[5])
    end
    return 2
  end
end
return 0
