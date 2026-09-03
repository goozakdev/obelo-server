// A QR Code encoder, vendored (ADR-0055 §2: the invite is shown "as copyable
// text and as a QR code").
//
// VENDORED, NOT A DEPENDENCY, and deliberately so. The web app ships four
// runtime dependencies; a QR encoder is ~300 lines of table-driven arithmetic
// with no I/O, no DOM and no versioning risk, and the alternatives were a fifth
// npm package or a CDN script — and a CDN is out of the question for a fully
// self-hosted Server (ADR-0001: nothing this app renders may need the internet).
//
// The algorithm is the ISO/IEC 18004 one, in the shape Project Nayuki's
// public-domain reference implementation states it; only BYTE mode is
// implemented, because the only thing this app ever encodes is one
// `obelo-link:` string of base64url and ASCII punctuation. Numeric and
// alphanumeric modes would make the symbol smaller for other inputs and are
// dead weight here.
//
// The output is a boolean matrix. Nothing here touches the DOM — QrSvg.tsx
// renders it.

/** Error-correction level. `L` (~7% recovery) is the default: the invite string
 * is ~350 bytes, and every rung above L costs a version or two of symbol size
 * for a code that is read once, on a bright screen, from a phone held still. */
export type EccLevel = "L" | "M" | "Q" | "H";

/** A rendered QR symbol: a square matrix of dark (`true`) / light modules, its
 * side length in modules, and the version (1–40) that was needed. */
export interface QrMatrix {
  size: number;
  version: number;
  modules: boolean[][];
}

/** Thrown when the text cannot fit in a version-40 symbol at the requested
 * level. The caller decides what to show; there is no smaller symbol to fall
 * back to. */
export class QrTooLongError extends Error {
  constructor(bytes: number) {
    super(`text is ${bytes} bytes, too long for a QR code`);
    this.name = "QrTooLongError";
  }
}

// --- The ISO/IEC 18004 tables ----------------------------------------------
//
// Indexed [ecc][version]; index 0 of each row is padding (there is no version 0)
// and is never read.

const ECC_ORDER: EccLevel[] = ["L", "M", "Q", "H"];

/** The two-bit field written into the format information — NOT the index above.
 * The standard numbers the levels in a different order from the one everybody
 * tabulates them in, and conflating the two is the classic way to produce a
 * symbol that is perfectly valid and reads as the wrong error-correction level
 * (so it decodes to noise). */
const ECC_FORMAT_BITS: Record<EccLevel, number> = { L: 1, M: 0, Q: 3, H: 2 };

// prettier-ignore
const ECC_CODEWORDS_PER_BLOCK: number[][] = [
  // 0   1   2   3   4   5   6   7   8   9  10  11  12  13  14  15  16  17  18  19  20  21  22  23  24  25  26  27  28  29  30  31  32  33  34  35  36  37  38  39  40
  [ -1,  7, 10, 15, 20, 26, 18, 20, 24, 30, 18, 20, 24, 26, 30, 22, 24, 28, 30, 28, 28, 28, 28, 30, 30, 26, 28, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30], // L
  [ -1, 10, 16, 26, 18, 24, 16, 18, 22, 22, 26, 30, 22, 22, 24, 24, 28, 28, 26, 26, 26, 26, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28], // M
  [ -1, 13, 22, 18, 26, 18, 24, 18, 22, 20, 24, 28, 26, 24, 20, 30, 24, 28, 28, 26, 30, 28, 30, 30, 30, 30, 28, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30], // Q
  [ -1, 17, 28, 22, 16, 22, 28, 26, 26, 24, 28, 24, 28, 22, 24, 24, 30, 28, 28, 26, 28, 30, 24, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30], // H
];

// prettier-ignore
const ECC_BLOCKS: number[][] = [
  // 0  1  2  3  4  5  6  7  8  9 10  11  12  13  14  15  16  17  18  19  20  21  22  23  24  25  26  27  28  29  30  31  32  33  34  35  36  37  38  39  40
  [ -1, 1, 1, 1, 1, 1, 2, 2, 2, 2, 4,  4,  4,  4,  4,  6,  6,  6,  6,  7,  8,  8,  9,  9, 10, 12, 12, 12, 13, 14, 15, 16, 17, 18, 19, 19, 20, 21, 22, 24, 25], // L
  [ -1, 1, 1, 1, 2, 2, 4, 4, 4, 5, 5,  5,  8,  9,  9, 10, 10, 11, 13, 14, 16, 17, 17, 18, 20, 21, 23, 25, 26, 28, 29, 31, 33, 35, 37, 38, 40, 43, 45, 47, 49], // M
  [ -1, 1, 1, 2, 2, 4, 4, 6, 6, 8, 8,  8, 10, 12, 16, 12, 17, 16, 18, 21, 20, 23, 23, 25, 27, 29, 34, 34, 35, 38, 40, 43, 45, 48, 51, 53, 56, 59, 62, 65, 68], // Q
  [ -1, 1, 1, 2, 4, 4, 4, 5, 5, 8, 8, 11, 11, 16, 16, 18, 16, 19, 21, 25, 25, 25, 34, 30, 32, 35, 37, 40, 42, 45, 48, 51, 54, 57, 60, 63, 66, 70, 74, 77, 81], // H
];

const PENALTY_N1 = 3;
const PENALTY_N2 = 3;
const PENALTY_N3 = 40;
const PENALTY_N4 = 10;

// --- Capacity ---------------------------------------------------------------

/** Every module of a symbol that is not a function pattern or format/version
 * information — i.e. the data area, in bits, before error correction. */
function rawDataModules(version: number): number {
  let result = (16 * version + 128) * version + 64;
  if (version >= 2) {
    const numAlign = Math.floor(version / 7) + 2;
    result -= (25 * numAlign - 10) * numAlign - 55;
    if (version >= 7) result -= 36;
  }
  return result;
}

/** How many 8-bit data codewords a (version, level) pair carries once the error
 * correction blocks are subtracted. Exported for the encoder's own round-trip
 * test, which walks the symbol back to the string it came from. */
export function dataCodewordCount(version: number, ecc: EccLevel): number {
  const e = ECC_ORDER.indexOf(ecc);
  return (
    Math.floor(rawDataModules(version) / 8) -
    ECC_CODEWORDS_PER_BLOCK[e][version] * ECC_BLOCKS[e][version]
  );
}

/** The byte-mode character-count field is 8 bits up to version 9 and 16 bits
 * from version 10 — which is why the version has to be chosen by trying, not by
 * a single capacity division. */
function charCountBits(version: number): number {
  return version <= 9 ? 8 : 16;
}

// --- GF(256) and Reed-Solomon ----------------------------------------------

/** Multiply in GF(2^8) modulo the QR primitive polynomial x^8+x^4+x^3+x^2+1. */
function gfMul(x: number, y: number): number {
  let z = 0;
  for (let i = 7; i >= 0; i--) {
    z = (z << 1) ^ ((z >>> 7) * 0x11d);
    z ^= ((y >>> i) & 1) * x;
  }
  return z & 0xff;
}

/** The generator polynomial's coefficients for `degree` error-correction
 * codewords, highest power first and with the leading 1 left implicit. */
function rsDivisor(degree: number): Uint8Array {
  const result = new Uint8Array(degree);
  result[degree - 1] = 1;
  let root = 1;
  for (let i = 0; i < degree; i++) {
    for (let j = 0; j < degree; j++) {
      result[j] = gfMul(result[j], root);
      if (j + 1 < degree) result[j] ^= result[j + 1];
    }
    root = gfMul(root, 0x02);
  }
  return result;
}

function rsRemainder(data: Uint8Array | number[], divisor: Uint8Array): number[] {
  const result = new Array<number>(divisor.length).fill(0);
  for (const b of data) {
    const factor = b ^ result[0];
    result.shift();
    result.push(0);
    for (let i = 0; i < result.length; i++) {
      result[i] ^= gfMul(divisor[i], factor);
    }
  }
  return result;
}

// --- Bit stream -------------------------------------------------------------

class BitBuffer {
  readonly bits: number[] = [];

  append(value: number, length: number): void {
    for (let i = length - 1; i >= 0; i--) {
      this.bits.push((value >>> i) & 1);
    }
  }
}

/** UTF-8 bytes of the text. The invite string is ASCII, but encoding the bytes
 * rather than the code units means a name with an apostrophe or an accent in it
 * cannot silently produce a different string on the other side. */
function utf8Bytes(text: string): Uint8Array {
  return new TextEncoder().encode(text);
}

/** Mode indicator, character count, payload, terminator, and the alternating
 * 0xEC/0x11 padding, as one array of data codewords for the chosen version. */
function dataCodewords(bytes: Uint8Array, version: number, ecc: EccLevel): number[] {
  const capacityBits = dataCodewordCount(version, ecc) * 8;
  const bb = new BitBuffer();
  bb.append(0b0100, 4); // byte mode
  bb.append(bytes.length, charCountBits(version));
  for (const b of bytes) bb.append(b, 8);

  // Terminator (up to four zeros), then zeros to the next byte boundary.
  bb.append(0, Math.min(4, capacityBits - bb.bits.length));
  bb.append(0, (8 - (bb.bits.length % 8)) % 8);

  const words: number[] = [];
  for (let i = 0; i < bb.bits.length; i += 8) {
    let w = 0;
    for (let j = 0; j < 8; j++) w = (w << 1) | bb.bits[i + j];
    words.push(w);
  }
  for (let pad = 0xec; words.length * 8 < capacityBits; pad ^= 0xec ^ 0x11) {
    words.push(pad);
  }
  return words;
}

/** Split the data codewords into blocks, append each block's error correction,
 * and INTERLEAVE the result — a burst of damage then lands across many blocks
 * instead of destroying one. */
function addEccAndInterleave(data: number[], version: number, ecc: EccLevel): number[] {
  const e = ECC_ORDER.indexOf(ecc);
  const numBlocks = ECC_BLOCKS[e][version];
  const blockEccLen = ECC_CODEWORDS_PER_BLOCK[e][version];
  const rawCodewords = Math.floor(rawDataModules(version) / 8);
  const numShortBlocks = numBlocks - (rawCodewords % numBlocks);
  const shortBlockDataLen = Math.floor(rawCodewords / numBlocks) - blockEccLen;

  const divisor = rsDivisor(blockEccLen);
  const blocks: number[][] = [];
  for (let i = 0, k = 0; i < numBlocks; i++) {
    const dat = data.slice(
      k,
      k + shortBlockDataLen + (i < numShortBlocks ? 0 : 1),
    );
    k += dat.length;
    const parity = rsRemainder(dat, divisor);
    // Short blocks get a placeholder byte so every block is the same length for
    // the interleave below; it is skipped rather than emitted.
    if (i < numShortBlocks) dat.push(0);
    blocks.push(dat.concat(parity));
  }

  const result: number[] = [];
  for (let i = 0; i < blocks[0].length; i++) {
    blocks.forEach((block, j) => {
      if (i !== shortBlockDataLen || j >= numShortBlocks) result.push(block[i]);
    });
  }
  return result;
}

// --- The symbol -------------------------------------------------------------

class Symbol_ {
  readonly size: number;
  readonly modules: boolean[][];
  /** Which modules are function patterns (finders, timing, alignment, format,
   * version) — the ones the data must skip and the mask must not touch. */
  private readonly isFunction: boolean[][];

  constructor(readonly version: number, readonly ecc: EccLevel) {
    this.size = version * 4 + 17;
    this.modules = Array.from({ length: this.size }, () =>
      new Array<boolean>(this.size).fill(false),
    );
    this.isFunction = Array.from({ length: this.size }, () =>
      new Array<boolean>(this.size).fill(false),
    );
  }

  private setFunction(x: number, y: number, dark: boolean): void {
    this.modules[y][x] = dark;
    this.isFunction[y][x] = true;
  }

  drawFunctionPatterns(): void {
    for (let i = 0; i < this.size; i++) {
      this.setFunction(6, i, i % 2 === 0);
      this.setFunction(i, 6, i % 2 === 0);
    }
    this.drawFinder(3, 3);
    this.drawFinder(this.size - 4, 3);
    this.drawFinder(3, this.size - 4);

    const pos = alignmentPositions(this.version);
    for (let i = 0; i < pos.length; i++) {
      for (let j = 0; j < pos.length; j++) {
        // The three corners already hold finder patterns.
        const corner =
          (i === 0 && j === 0) ||
          (i === 0 && j === pos.length - 1) ||
          (i === pos.length - 1 && j === 0);
        if (!corner) this.drawAlignment(pos[i], pos[j]);
      }
    }

    this.drawFormatBits(0); // Placeholder; rewritten once the mask is chosen.
    this.drawVersionBits();
  }

  private drawFinder(x: number, y: number): void {
    for (let dy = -4; dy <= 4; dy++) {
      for (let dx = -4; dx <= 4; dx++) {
        const dist = Math.max(Math.abs(dx), Math.abs(dy));
        const xx = x + dx;
        const yy = y + dy;
        if (xx >= 0 && xx < this.size && yy >= 0 && yy < this.size) {
          this.setFunction(xx, yy, dist !== 2 && dist !== 4);
        }
      }
    }
  }

  private drawAlignment(x: number, y: number): void {
    for (let dy = -2; dy <= 2; dy++) {
      for (let dx = -2; dx <= 2; dx++) {
        this.setFunction(
          x + dx,
          y + dy,
          Math.max(Math.abs(dx), Math.abs(dy)) !== 1,
        );
      }
    }
  }

  /** The 15-bit format information — error-correction level and mask — written
   * twice, BCH-encoded and XORed with 0x5412. Both copies must agree; a decoder
   * that reads the wrong mask reads the wrong symbol. */
  drawFormatBits(mask: number): void {
    const data = (ECC_FORMAT_BITS[this.ecc] << 3) | mask;
    let rem = data;
    for (let i = 0; i < 10; i++) rem = (rem << 1) ^ ((rem >>> 9) * 0x537);
    const bits = (((data << 10) | rem) ^ 0x5412) >>> 0;
    const bit = (i: number) => ((bits >>> i) & 1) !== 0;

    for (let i = 0; i <= 5; i++) this.setFunction(8, i, bit(i));
    this.setFunction(8, 7, bit(6));
    this.setFunction(8, 8, bit(7));
    this.setFunction(7, 8, bit(8));
    for (let i = 9; i < 15; i++) this.setFunction(14 - i, 8, bit(i));

    for (let i = 0; i < 8; i++) this.setFunction(this.size - 1 - i, 8, bit(i));
    for (let i = 8; i < 15; i++) this.setFunction(8, this.size - 15 + i, bit(i));
    this.setFunction(8, this.size - 8, true); // The one always-dark module.
  }

  /** Version information, present only from version 7 up. */
  private drawVersionBits(): void {
    if (this.version < 7) return;
    let rem = this.version;
    for (let i = 0; i < 12; i++) rem = (rem << 1) ^ ((rem >>> 11) * 0x1f25);
    const bits = (this.version << 12) | rem;
    for (let i = 0; i < 18; i++) {
      const dark = ((bits >>> i) & 1) !== 0;
      const a = this.size - 11 + (i % 3);
      const b = Math.floor(i / 3);
      this.setFunction(a, b, dark);
      this.setFunction(b, a, dark);
    }
  }

  /** Lay the interleaved codewords into the zigzag of two-module columns that
   * runs up and down the symbol, skipping every function module. */
  drawCodewords(data: number[]): void {
    let i = 0;
    for (let right = this.size - 1; right >= 1; right -= 2) {
      if (right === 6) right = 5; // The vertical timing pattern's column.
      for (let vert = 0; vert < this.size; vert++) {
        for (let j = 0; j < 2; j++) {
          const x = right - j;
          const upward = ((right + 1) & 2) === 0;
          const y = upward ? this.size - 1 - vert : vert;
          if (!this.isFunction[y][x] && i < data.length * 8) {
            this.modules[y][x] = ((data[i >>> 3] >>> (7 - (i & 7))) & 1) !== 0;
            i++;
          }
        }
      }
    }
  }

  /** XOR one of the eight mask patterns over the data area. Its own inverse, so
   * the same call undoes it — which is how the eight candidates are scored. */
  applyMask(mask: number): void {
    for (let y = 0; y < this.size; y++) {
      for (let x = 0; x < this.size; x++) {
        if (this.isFunction[y][x]) continue;
        let invert = false;
        switch (mask) {
          case 0: invert = (x + y) % 2 === 0; break;
          case 1: invert = y % 2 === 0; break;
          case 2: invert = x % 3 === 0; break;
          case 3: invert = (x + y) % 3 === 0; break;
          case 4: invert = (Math.floor(x / 3) + Math.floor(y / 2)) % 2 === 0; break;
          case 5: invert = ((x * y) % 2) + ((x * y) % 3) === 0; break;
          case 6: invert = (((x * y) % 2) + ((x * y) % 3)) % 2 === 0; break;
          case 7: invert = (((x + y) % 2) + ((x * y) % 3)) % 2 === 0; break;
        }
        if (invert) this.modules[y][x] = !this.modules[y][x];
      }
    }
  }

  /** The standard's four penalty rules. Only the RANKING matters: every mask
   * produces a decodable symbol, and this picks the one a camera has the easiest
   * time with. */
  penalty(): number {
    let result = 0;
    const size = this.size;

    for (let y = 0; y < size; y++) {
      let runColor = false;
      let runLen = 0;
      const history = [0, 0, 0, 0, 0, 0, 0];
      for (let x = 0; x < size; x++) {
        if (this.modules[y][x] === runColor) {
          runLen++;
          if (runLen === 5) result += PENALTY_N1;
          else if (runLen > 5) result++;
        } else {
          this.addRunToHistory(runLen, history);
          if (!runColor) result += countFinderLike(history) * PENALTY_N3;
          runColor = this.modules[y][x];
          runLen = 1;
        }
      }
      result += this.terminateRun(runColor, runLen, history) * PENALTY_N3;
    }
    for (let x = 0; x < size; x++) {
      let runColor = false;
      let runLen = 0;
      const history = [0, 0, 0, 0, 0, 0, 0];
      for (let y = 0; y < size; y++) {
        if (this.modules[y][x] === runColor) {
          runLen++;
          if (runLen === 5) result += PENALTY_N1;
          else if (runLen > 5) result++;
        } else {
          this.addRunToHistory(runLen, history);
          if (!runColor) result += countFinderLike(history) * PENALTY_N3;
          runColor = this.modules[y][x];
          runLen = 1;
        }
      }
      result += this.terminateRun(runColor, runLen, history) * PENALTY_N3;
    }

    for (let y = 0; y < size - 1; y++) {
      for (let x = 0; x < size - 1; x++) {
        const c = this.modules[y][x];
        if (
          c === this.modules[y][x + 1] &&
          c === this.modules[y + 1][x] &&
          c === this.modules[y + 1][x + 1]
        ) {
          result += PENALTY_N2;
        }
      }
    }

    let dark = 0;
    for (const row of this.modules) for (const c of row) if (c) dark++;
    const total = size * size;
    const k = Math.ceil(Math.abs(dark * 20 - total * 10) / total) - 1;
    return result + k * PENALTY_N4;
  }

  private addRunToHistory(runLen: number, history: number[]): void {
    if (history[0] === 0) runLen += this.size; // Add the light margin.
    history.pop();
    history.unshift(runLen);
  }

  private terminateRun(
    runColor: boolean,
    runLen: number,
    history: number[],
  ): number {
    if (runColor) {
      this.addRunToHistory(runLen, history);
      runLen = 0;
    }
    this.addRunToHistory(runLen + this.size, history);
    return countFinderLike(history);
  }
}

/** The 1:1:3:1:1 finder-like pattern the third penalty rule counts. */
function countFinderLike(history: number[]): number {
  const n = history[1];
  const core =
    n > 0 &&
    history[2] === n &&
    history[3] === n * 3 &&
    history[4] === n &&
    history[5] === n;
  return (
    (core && history[0] >= n * 4 && history[6] >= n ? 1 : 0) +
    (core && history[6] >= n * 4 && history[0] >= n ? 1 : 0)
  );
}

/** Where the alignment patterns sit for a version, as row/column coordinates.
 * Exported for the round-trip test, which has to know which modules are
 * function patterns to walk the data back out. */
export function alignmentPositions(version: number): number[] {
  if (version === 1) return [];
  const numAlign = Math.floor(version / 7) + 2;
  const step =
    version === 32
      ? 26
      : Math.ceil((version * 4 + 4) / (numAlign * 2 - 2)) * 2;
  const result = [6];
  for (let pos = version * 4 + 10; result.length < numAlign; pos -= step) {
    result.splice(1, 0, pos);
  }
  return result;
}

// --- The one entry point ----------------------------------------------------

/** Encode `text` as the smallest QR symbol that fits it at `ecc`.
 *
 * Throws {@link QrTooLongError} when even version 40 is too small — the caller
 * has to say something, because there is no smaller symbol to fall back to. */
export function encodeQr(text: string, ecc: EccLevel = "L"): QrMatrix {
  const bytes = utf8Bytes(text);

  let version = 0;
  for (let v = 1; v <= 40; v++) {
    const capacityBits = dataCodewordCount(v, ecc) * 8;
    if (4 + charCountBits(v) + bytes.length * 8 <= capacityBits) {
      version = v;
      break;
    }
  }
  if (version === 0) throw new QrTooLongError(bytes.length);

  const sym = new Symbol_(version, ecc);
  sym.drawFunctionPatterns();
  sym.drawCodewords(
    addEccAndInterleave(dataCodewords(bytes, version, ecc), version, ecc),
  );

  // Try all eight masks and keep the least-penalised one.
  let bestMask = 0;
  let bestPenalty = Infinity;
  for (let mask = 0; mask < 8; mask++) {
    sym.applyMask(mask);
    sym.drawFormatBits(mask);
    const p = sym.penalty();
    if (p < bestPenalty) {
      bestPenalty = p;
      bestMask = mask;
    }
    sym.applyMask(mask); // XOR is its own inverse.
  }
  sym.applyMask(bestMask);
  sym.drawFormatBits(bestMask);

  return { size: sym.size, version, modules: sym.modules };
}
