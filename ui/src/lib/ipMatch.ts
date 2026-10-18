/**
 * Caller-address matching for the API key page. It mirrors the server's
 * auth.IPAllowed so the table can tell the operator whether the address a key
 * was last used from still falls inside its allowlist; the server remains the
 * only authority that admits or rejects a request.
 */

/** Strips a port and an IPv6 zone from a recorded caller address. */
export function hostFromAddress(value: string): string {
  let address = (value ?? "").trim();
  if (address.startsWith("[")) {
    const end = address.indexOf("]");
    if (end > 0) address = address.slice(1, end);
  } else if (address.split(":").length === 2 && address.includes(".")) {
    // host:port for an IPv4 caller; a bare IPv6 address has more colons.
    address = address.slice(0, address.indexOf(":"));
  }
  const zone = address.indexOf("%");
  return zone >= 0 ? address.slice(0, zone) : address;
}

function ipv4ToInt(address: string): number | null {
  const parts = address.split(".");
  if (parts.length !== 4) return null;
  let value = 0;
  for (const part of parts) {
    if (!/^\d{1,3}$/.test(part)) return null;
    const octet = Number(part);
    if (octet > 255) return null;
    value = value * 256 + octet;
  }
  return value;
}

function ipv6ToBytes(address: string): number[] | null {
  if (!address.includes(":")) return null;
  const [head, tail] = address.includes("::") ? address.split("::", 2) : [address, undefined];
  const headParts = head === "" ? [] : head.split(":");
  const tailParts = tail === undefined || tail === "" ? [] : tail.split(":");
  const missing = 8 - (headParts.length + tailParts.length);
  if (tail === undefined && missing !== 0) return null;
  if (missing < 0) return null;
  const groups = [...headParts, ...new Array(Math.max(0, missing)).fill("0"), ...tailParts];
  const bytes: number[] = [];
  for (const group of groups) {
    if (!/^[0-9a-fA-F]{1,4}$/.test(group)) return null;
    const value = parseInt(group, 16);
    bytes.push((value >> 8) & 0xff, value & 0xff);
  }
  return bytes.length === 16 ? bytes : null;
}

/**
 * Reports whether a caller address falls inside one allowlist entry. The entry
 * is a bare address or a CIDR block; anything that cannot be parsed matches
 * nothing, exactly like the server's check.
 */
export function ipMatchesEntry(entry: string, caller: string): boolean {
  const candidate = hostFromAddress(caller);
  const allowed = (entry ?? "").trim();
  if (candidate === "" || allowed === "") return false;

  if (allowed.includes("/")) {
    const [network, prefixText] = allowed.split("/", 2);
    const prefix = Number(prefixText);
    if (!Number.isInteger(prefix) || prefix < 0) return false;
    const networkBytes = ipv6ToBytes(hostFromAddress(network));
    const callerBytes = ipv6ToBytes(candidate);
    if (networkBytes && callerBytes && prefix <= 128) {
      return prefixBytesEqual(networkBytes, callerBytes, prefix);
    }
    const networkValue = ipv4ToInt(network);
    const callerValue = ipv4ToInt(candidate);
    if (networkValue === null || callerValue === null || prefix > 32) return false;
    const mask = prefix === 0 ? 0 : (0xffffffff << (32 - prefix)) >>> 0;
    return (networkValue & mask) === (callerValue & mask);
  }

  // Bare addresses compare canonically: an IPv6 caller written in full and an
  // entry written compressed are the same address.
  const allowedBytes = ipv6ToBytes(hostFromAddress(allowed));
  const callerBytes = ipv6ToBytes(candidate);
  if (allowedBytes && callerBytes) return allowedBytes.every((byte, index) => byte === callerBytes[index]);
  const allowedValue = ipv4ToInt(allowed);
  const callerValue = ipv4ToInt(candidate);
  if (allowedValue !== null && callerValue !== null) return allowedValue === callerValue;
  return allowed === candidate;
}

function prefixBytesEqual(left: number[], right: number[], prefix: number): boolean {
  const wholeBytes = Math.floor(prefix / 8);
  for (let index = 0; index < wholeBytes; index += 1) {
    if (left[index] !== right[index]) return false;
  }
  const remainder = prefix % 8;
  if (remainder === 0) return true;
  const mask = (0xff << (8 - remainder)) & 0xff;
  return (left[wholeBytes] & mask) === (right[wholeBytes] & mask);
}
