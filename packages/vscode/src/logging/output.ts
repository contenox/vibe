import * as vscode from "vscode";

export class ContenoxOutput implements vscode.Disposable {
  private readonly output = vscode.window.createOutputChannel("Contenox", { log: true });
  private readonly protocolOutput = vscode.window.createOutputChannel("Contenox Protocol", { log: true });

  public info(message: string): void {
    this.output.info(message);
  }

  public warn(message: string): void {
    this.output.warn(message);
  }

  public error(message: string): void {
    this.output.error(message);
  }

  public protocol(message: string): void {
    this.protocolOutput.trace(message);
  }

  public show(): void {
    this.output.show();
  }

  public dispose(): void {
    this.output.dispose();
    this.protocolOutput.dispose();
  }
}
