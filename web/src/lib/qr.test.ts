import { describe, it, expect } from "vitest";
import {
  encodeQr,
  QrTooLongError,
  alignmentPositions,
  dataCodewordCount,
  type EccLevel,
  type QrMatrix,
} from "./qr";

// The acceptance criterion for issue 04 is "the QR decodes to EXACTLY the string
// in the field", so these tests decode it.
//
// `readBack` below is a QR reader minus the two things a camera needs and a unit
// test does not: it does not locate the symbol in an image, and it does not run
// Reed-Solomon error CORRECTION (nothing here is damaged). Everything else is
// the real reverse of the encoder — read the format information to learn the
// mask, un-mask, walk the zigzag, de-interleave the blocks, drop the parity, and
// decode the byte-mode segment. That is what makes it evidence rather than a
// restatement: a wrong mask, a wrong format polynomial, a wrong block split or a
// transposed module placement all break it.

const ECC_ORDER: EccLevel[] = ["L", "M", "Q", "H"];
const ECC_FROM_FORMAT: Record<number, EccLevel> = { 1: "L", 0: "M", 3: "Q", 2: "H" };

// The same two tables the encoder carries, restated here on purpose: a decoder
// that imported the encoder's tables could not catch a wrong table.
// prettier-ignore
const ECC_CODEWORDS_PER_BLOCK: number[][] = [
  [ -1,  7, 10, 15, 20, 26, 18, 20, 24, 30, 18, 20, 24, 26, 30, 22, 24, 28, 30, 28, 28, 28, 28, 30, 30, 26, 28, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30],
  [ -1, 10, 16, 26, 18, 24, 16, 18, 22, 22, 26, 30, 22, 22, 24, 24, 28, 28, 26, 26, 26, 26, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28],
  [ -1, 13, 22, 18, 26, 18, 24, 18, 22, 20, 24, 28, 26, 24, 20, 30, 24, 28, 28, 26, 30, 28, 30, 30, 30, 30, 28, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30],
  [ -1, 17, 28, 22, 16, 22, 28, 26, 26, 24, 28, 24, 28, 22, 24, 24, 30, 28, 28, 26, 28, 30, 24, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30],
];
// prettier-ignore
const ECC_BLOCKS: number[][] = [
  [ -1, 1, 1, 1, 1, 1, 2, 2, 2, 2, 4,  4,  4,  4,  4,  6,  6,  6,  6,  7,  8,  8,  9,  9, 10, 12, 12, 12, 13, 14, 15, 16, 17, 18, 19, 19, 20, 21, 22, 24, 25],
  [ -1, 1, 1, 1, 2, 2, 4, 4, 4, 5, 5,  5,  8,  9,  9, 10, 10, 11, 13, 14, 16, 17, 17, 18, 20, 21, 23, 25, 26, 28, 29, 31, 33, 35, 37, 38, 40, 43, 45, 47, 49],
  [ -1, 1, 1, 2, 2, 4, 4, 6, 6, 8, 8,  8, 10, 12, 16, 12, 17, 16, 18, 21, 20, 23, 23, 25, 27, 29, 34, 34, 35, 38, 40, 43, 45, 48, 51, 53, 56, 59, 62, 65, 68],
  [ -1, 1, 1, 2, 4, 4, 4, 5, 5, 8, 8, 11, 11, 16, 16, 18, 16, 19, 21, 25, 25, 25, 34, 30, 32, 35, 37, 40, 42, 45, 48, 51, 54, 57, 60, 63, 66, 70, 74, 77, 81],
];

/** Rebuild the "this module is a function pattern" map from the version alone —
 * the same way a decoder does, from the geometry, not from the encoder. */
function functionMap(size: number, version: number): boolean[][] {
  const isFn = Array.from({ length: size }, () =>
    new Array<boolean>(size).fill(false),
  );
  const mark = (x: number, y: number) => {
    if (x >= 0 && x < size && y >= 0 && y < size) isFn[y][x] = true;
  };

  for (let i = 0; i < size; i++) {
    mark(6, i);
    mark(i, 6);
  }
  for (const [fx, fy] of [
    [3, 3],
    [size - 4, 3],
    [3, size - 4],
  ]) {
    for (let dy = -4; dy <= 4; dy++)
      for (let dx = -4; dx <= 4; dx++) mark(fx + dx, fy + dy);
  }
  const pos = alignmentPositions(version);
  for (let i = 0; i < pos.length; i++) {
    for (let j = 0; j < pos.length; j++) {
      const corner =
        (i === 0 && j === 0) ||
        (i === 0 && j === pos.length - 1) ||
        (i === pos.length - 1 && j === 0);
      if (corner) continue;
      for (let dy = -2; dy <= 2; dy++)
        for (let dx = -2; dx <= 2; dx++) mark(pos[i] + dx, pos[j] + dy);
    }
  }
  // Format information (both copies) and the always-dark module.
  for (let i = 0; i <= 8; i++) {
    mark(8, i);
    mark(i, 8);
  }
  for (let i = 0; i < 8; i++) mark(size - 1 - i, 8);
  for (let i = 0; i < 8; i++) mark(8, size - 1 - i);
  // Version information.
  if (version >= 7) {
    for (let i = 0; i < 18; i++) {
      const a = size - 11 + (i % 3);
      const b = Math.floor(i / 3);
      mark(a, b);
      mark(b, a);
    }
  }
  return isFn;
}

/** Read the 15-bit format information back out of the top-left copy and undo the
 * 0x5412 mask, yielding the error-correction level and the mask number the
 * encoder chose. */
function readFormat(m: QrMatrix): { ecc: EccLevel; mask: number } {
  const bit = (x: number, y: number) => (m.modules[y][x] ? 1 : 0);
  let bits = 0;
  const read: number[] = [];
  for (let i = 0; i <= 5; i++) read.push(bit(8, i));
  read.push(bit(8, 7));
  read.push(bit(8, 8));
  read.push(bit(7, 8));
  for (let i = 9; i < 15; i++) read.push(bit(14 - i, 8));
  for (let i = 0; i < 15; i++) bits |= read[i] << i;
  const data = (bits ^ 0x5412) >>> 10;
  return { ecc: ECC_FROM_FORMAT[(data >>> 3) & 3], mask: data & 7 };
}

function maskInverts(mask: number, x: number, y: number): boolean {
  switch (mask) {
    case 0: return (x + y) % 2 === 0;
    case 1: return y % 2 === 0;
    case 2: return x % 3 === 0;
    case 3: return (x + y) % 3 === 0;
    case 4: return (Math.floor(x / 3) + Math.floor(y / 2)) % 2 === 0;
    case 5: return ((x * y) % 2) + ((x * y) % 3) === 0;
    case 6: return (((x * y) % 2) + ((x * y) % 3)) % 2 === 0;
    default: return (((x + y) % 2) + ((x * y) % 3)) % 2 === 0;
  }
}

/** The full reverse of the encoder: symbol → the string it carries. */
function readBack(m: QrMatrix): string {
  const { size, version } = m;
  const { ecc, mask } = readFormat(m);
  const isFn = functionMap(size, version);

  // Un-mask, then walk the same zigzag the encoder wrote.
  const bits: number[] = [];
  for (let right = size - 1; right >= 1; right -= 2) {
    if (right === 6) right = 5;
    for (let vert = 0; vert < size; vert++) {
      for (let j = 0; j < 2; j++) {
        const x = right - j;
        const upward = ((right + 1) & 2) === 0;
        const y = upward ? size - 1 - vert : vert;
        if (isFn[y][x]) continue;
        const dark = m.modules[y][x] !== maskInverts(mask, x, y);
        bits.push(dark ? 1 : 0);
      }
    }
  }
  const codewords: number[] = [];
  for (let i = 0; i + 8 <= bits.length; i += 8) {
    let w = 0;
    for (let j = 0; j < 8; j++) w = (w << 1) | bits[i + j];
    codewords.push(w);
  }

  // De-interleave back into blocks and drop the parity.
  const e = ECC_ORDER.indexOf(ecc);
  const numBlocks = ECC_BLOCKS[e][version];
  const blockEccLen = ECC_CODEWORDS_PER_BLOCK[e][version];
  const total = dataCodewordCount(version, ecc) + blockEccLen * numBlocks;
  const numShortBlocks = numBlocks - (total % numBlocks);
  const shortBlockDataLen = Math.floor(total / numBlocks) - blockEccLen;

  const blocks: number[][] = Array.from({ length: numBlocks }, () => []);
  let k = 0;
  const maxLen = shortBlockDataLen + 1 + blockEccLen;
  for (let i = 0; i < maxLen; i++) {
    for (let j = 0; j < numBlocks; j++) {
      if (i === shortBlockDataLen && j < numShortBlocks) continue;
      if (i >= (j < numShortBlocks ? shortBlockDataLen : shortBlockDataLen + 1) + blockEccLen)
        continue;
      blocks[j].push(codewords[k++]);
    }
  }
  const data: number[] = [];
  for (let j = 0; j < numBlocks; j++) {
    const dataLen = j < numShortBlocks ? shortBlockDataLen : shortBlockDataLen + 1;
    data.push(...blocks[j].slice(0, dataLen));
  }

  // Decode the byte-mode segment.
  const stream: number[] = [];
  for (const w of data) for (let i = 7; i >= 0; i--) stream.push((w >>> i) & 1);
  let p = 0;
  const take = (n: number) => {
    let v = 0;
    for (let i = 0; i < n; i++) v = (v << 1) | stream[p++];
    return v;
  };
  const mode = take(4);
  if (mode !== 0b0100) throw new Error(`mode ${mode}, want byte mode`);
  const len = take(version <= 9 ? 8 : 16);
  const bytes = new Uint8Array(len);
  for (let i = 0; i < len; i++) bytes[i] = take(8);
  return new TextDecoder().decode(bytes);
}

const SAMPLE_INVITE =
  "obelo-link:eyJ2IjoxLCJpZCI6IjNmNmExYzJlLTAwMDAtNDAwMC04MDAwLWFiY2RlZmFiY2RlZiIs" +
  "Im5hbWUiOiJCcmFuZG9uJ3Mgc2VydmVyIiwib3JpZ2lucyI6WyJodHRwOi8vb2JlbG8udGFpbDFhMmIu" +
  "dHMubmV0IiwiaHR0cHM6Ly9tZWRpYS5leGFtcGxlLm9yZyJdLCJjb2RlIjoiWTNSaGJHbHVaMmx1ZG1s" +
  "MFpXTnZaR1V4TWpNME5UWTNPRGxoWW1Oo0IiwiZXhwIjoiMjAyNi0wOS0wNFQxMjozNDo1NloifQ";

describe("encodeQr", () => {
  it("round-trips a short string", () => {
    expect(readBack(encodeQr("HELLO"))).toBe("HELLO");
  });

  it("round-trips a whole obelo-link: invite string, byte for byte", () => {
    const m = encodeQr(SAMPLE_INVITE);
    expect(readBack(m)).toBe(SAMPLE_INVITE);
    // Big enough to have needed version information and several ECC blocks —
    // i.e. the interleave and the version bits are actually being exercised.
    expect(m.version).toBeGreaterThanOrEqual(7);
  });

  it("round-trips at every error-correction level", () => {
    for (const ecc of ECC_ORDER) {
      const m = encodeQr(SAMPLE_INVITE, ecc);
      expect(readBack(m), `level ${ecc}`).toBe(SAMPLE_INVITE);
    }
  });

  it("round-trips across the version boundaries the format changes at", () => {
    // Version 9→10 is where the byte-mode character count grows from 8 bits to
    // 16, and 6→7 is where version information appears. Sweeping lengths walks
    // over both without having to name the versions.
    for (const len of [1, 8, 100, 154, 155, 156, 271, 400, 900, 1500]) {
      const text = "x".repeat(len);
      expect(readBack(encodeQr(text)), `length ${len}`).toBe(text);
    }
  });

  it("round-trips non-ASCII as UTF-8", () => {
    const text = "Brandon’s server — café";
    expect(readBack(encodeQr(text))).toBe(text);
  });

  it("grows the symbol with the text and stays square", () => {
    const small = encodeQr("HELLO");
    const big = encodeQr(SAMPLE_INVITE);
    expect(small.size).toBe(small.version * 4 + 17);
    expect(big.size).toBe(big.version * 4 + 17);
    expect(big.version).toBeGreaterThan(small.version);
    expect(big.modules).toHaveLength(big.size);
    expect(big.modules[0]).toHaveLength(big.size);
  });

  it("draws the three finder patterns", () => {
    const { modules, size } = encodeQr("HELLO");
    for (const [x, y] of [
      [0, 0],
      [size - 7, 0],
      [0, size - 7],
    ]) {
      // The 7x7 finder: dark ring, light ring, dark 3x3 core.
      expect(modules[y][x], `finder at ${x},${y}`).toBe(true);
      expect(modules[y + 1][x + 1]).toBe(false);
      expect(modules[y + 3][x + 3]).toBe(true);
    }
  });

  it("refuses a string no version-40 symbol can hold", () => {
    expect(() => encodeQr("x".repeat(5000))).toThrow(QrTooLongError);
  });
});
