import type { ApiError } from "../api/client";

/**
 * The one place a failed API call surfaces. It mirrors the CLI's error
 * contract — the message, then the remediation — so an operator reads the
 * same two lines whichever surface they are on
 * (`error:` / `hint:` in internal/clifmt).
 */
export function ErrorNotice({ error }: { error: ApiError }) {
  return (
    <div className="error-notice" id="error-notice" role="alert">
      <p className="error-notice__message">
        <span className="error-notice__icon" aria-hidden="true">
          ✕
        </span>
        <strong>error:</strong> {error.message}
      </p>
      <p className="error-notice__hint">
        <strong>hint:</strong> {error.remediation}
      </p>
    </div>
  );
}

export default ErrorNotice;
