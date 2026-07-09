import { ApiError, NetworkError } from "../api/client";

// Turns any thrown error from the API client into a human message for a form.
// ApiError carries the server's readable `message` (PRD user story 34);
// NetworkError means the server was unreachable; anything else is unexpected.
export function errorMessage(err: unknown): string {
  if (err instanceof ApiError) return err.message;
  if (err instanceof NetworkError) return "Could not reach the server. Is it running?";
  if (err instanceof Error) return err.message;
  return String(err);
}
