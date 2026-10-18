import { describe, expect, it } from "vitest";
import { hostFromAddress, ipMatchesEntry } from "./ipMatch";

describe("hostFromAddress", () => {
  it("strips a port from an IPv4 caller", () => {
    expect(hostFromAddress("192.168.100.10:9292")).toBe("192.168.100.10");
  });

  it("strips brackets, port and zone from an IPv6 caller", () => {
    expect(hostFromAddress("[::1]:443")).toBe("::1");
    expect(hostFromAddress("fe80::1%eth0")).toBe("fe80::1");
    expect(hostFromAddress("2001:db8::5")).toBe("2001:db8::5");
  });
});

describe("ipMatchesEntry", () => {
  it("matches a CIDR block", () => {
    expect(ipMatchesEntry("192.168.100.0/24", "192.168.100.10")).toBe(true);
    expect(ipMatchesEntry("192.168.100.0/24", "192.168.101.10")).toBe(false);
    expect(ipMatchesEntry("10.0.0.0/8", "10.4.5.6:1234")).toBe(true);
  });

  it("matches a bare address, including an IPv6 form written differently", () => {
    expect(ipMatchesEntry("203.0.113.4", "203.0.113.4")).toBe(true);
    expect(ipMatchesEntry("203.0.113.4", "203.0.113.5")).toBe(false);
    expect(ipMatchesEntry("2001:db8::1", "2001:0db8:0000:0000:0000:0000:0000:0001")).toBe(true);
    expect(ipMatchesEntry("2001:db8::/32", "2001:db8:1234::9")).toBe(true);
    expect(ipMatchesEntry("2001:db8::/32", "2001:db9::1")).toBe(false);
  });

  it("matches nothing for unparseable input", () => {
    expect(ipMatchesEntry("", "192.168.1.1")).toBe(false);
    expect(ipMatchesEntry("192.168.1.0/24", "")).toBe(false);
    expect(ipMatchesEntry("not-an-ip", "192.168.1.1")).toBe(false);
    expect(ipMatchesEntry("192.168.1.0/99", "192.168.1.1")).toBe(false);
  });
});
