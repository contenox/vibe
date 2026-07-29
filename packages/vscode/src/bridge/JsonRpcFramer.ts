import { Writable } from "node:stream";

const headerSeparator = Buffer.from("\r\n\r\n", "ascii");
const maxContentLength = 64 * 1024 * 1024;

export class JsonRpcFramer {
  private buffer = Buffer.alloc(0);
  private contentLength: number | undefined;

  public constructor(
    private readonly writer: Writable,
    private readonly onMessage: (message: unknown) => void,
    private readonly onError: (error: Error) => void,
  ) {}

  public accept(chunk: Buffer): void {
    this.buffer = Buffer.concat([this.buffer, chunk]);

    try {
      while (true) {
        if (this.contentLength === undefined) {
          const headerEnd = this.buffer.indexOf(headerSeparator);
          if (headerEnd === -1) {
            return;
          }
          const header = this.buffer.subarray(0, headerEnd).toString("ascii");
          this.buffer = this.buffer.subarray(headerEnd + headerSeparator.length);
          this.contentLength = parseContentLength(header);
        }

        if (this.buffer.length < this.contentLength) {
          return;
        }

        const payload = this.buffer.subarray(0, this.contentLength);
        this.buffer = this.buffer.subarray(this.contentLength);
        this.contentLength = undefined;

        this.onMessage(JSON.parse(payload.toString("utf8")) as unknown);
      }
    } catch (error) {
      this.buffer = Buffer.alloc(0);
      this.contentLength = undefined;
      this.onError(error instanceof Error ? error : new Error(String(error)));
    }
  }

  public send(message: unknown): void {
    const payload = Buffer.from(JSON.stringify(message), "utf8");
    const header = Buffer.from(`Content-Length: ${payload.length}\r\n\r\n`, "ascii");
    this.writer.write(Buffer.concat([header, payload]));
  }
}

function parseContentLength(header: string): number {
  for (const line of header.split(/\r?\n/)) {
    const [rawKey, ...rest] = line.split(":");
    if (rawKey.trim().toLowerCase() !== "content-length") {
      continue;
    }
    const rawValue = rest.join(":").trim();
    if (!/^\d+$/.test(rawValue)) {
      throw new Error(`Invalid Content-Length header: ${line}`);
    }
    const value = Number.parseInt(rawValue, 10);
    if (!Number.isFinite(value) || value < 0) {
      throw new Error(`Invalid Content-Length header: ${line}`);
    }
    if (value > maxContentLength) {
      throw new Error(`Invalid Content-Length header: ${line}`);
    }
    return value;
  }
  throw new Error("Missing Content-Length header");
}
