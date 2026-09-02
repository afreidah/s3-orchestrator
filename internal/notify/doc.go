// Package notify drains the notification_outbox table and delivers
// each pending event to its configured webhook target with retries
// and exponential backoff under an advisory lock for multi-instance
// safety.
package notify
