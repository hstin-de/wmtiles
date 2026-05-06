export class WMTError extends Error {
  override name = "WMTError";
  constructor(message?: string, options?: ErrorOptions) {
    super(message, options);
  }
}

export class FormatError extends WMTError {
  override name = "FormatError";
}

export class SourceError extends WMTError {
  override name = "SourceError";
}

export class UnknownVariableError extends WMTError {
  override name = "UnknownVariableError";
  readonly variableName: string;
  constructor(name: string) {
    super(`unknown variable: ${JSON.stringify(name)}`);
    this.variableName = name;
  }
}

export class TimeOutOfRangeError extends WMTError {
  override name = "TimeOutOfRangeError";
}
