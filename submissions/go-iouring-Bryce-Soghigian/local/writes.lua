-- Write-heavy mix: 85% like a random post, 15% create a post. Random user per request.
dofile("/s/tokens.lua")
local n = 0
request = function()
  n = n + 1
  local hdr = { ["Authorization"] = "Bearer " .. tokens[math.random(#tokens)], ["Content-Type"] = "application/json" }
  if math.random() < 0.15 then
    return wrk.format("POST", "/posts", hdr, '{"body":"wrk write ' .. n .. '"}')
  end
  return wrk.format("POST", "/posts/" .. math.random(1, 500000) .. "/like", hdr)
end
