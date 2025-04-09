// Function to convert timestamps to local timezone
function convertTimestampsToLocalTimezone() {
    document.querySelectorAll('.server-timestamp').forEach(function (element) {
        const serverTimestamp = element.getAttribute('data-timestamp');
        if (serverTimestamp) {
            try {
                // For Go timestamps like "2025-04-09 18:37:42.93998 +0200 CEST"
                // Extract the core date/time and timezone offset
                const match = serverTimestamp.match(/^(\d{4}-\d{2}-\d{2}\s\d{2}:\d{2}:\d{2})(?:\.\d+)?\s([+-]\d{4})/);

                if (match) {
                    // We have a timestamp with timezone info
                    const [, dateTime, offset] = match;
                    // Format: "2025-04-09T18:37:42+0200"
                    const isoString = dateTime.replace(' ', 'T') + offset;
                    const date = new Date(isoString);

                    if (!isNaN(date.getTime())) {
                        element.textContent = date.toLocaleString();
                        return;
                    }
                }

                // Fallback for other formats - just try to parse it directly
                const simpleDate = new Date(serverTimestamp);
                if (!isNaN(simpleDate.getTime())) {
                    element.textContent = simpleDate.toLocaleString();
                    return;
                }

                console.error("Could not parse date: ", serverTimestamp);
                element.textContent = serverTimestamp; // Fall back to original
            } catch (e) {
                console.error("Error parsing date: ", e);
                element.textContent = serverTimestamp; // Fall back to original
            }
        }
    });
}

// Run converter when DOM is loaded
document.addEventListener('DOMContentLoaded', convertTimestampsToLocalTimezone);

// Also run after HTMX content is loaded
document.addEventListener('htmx:afterSwap', convertTimestampsToLocalTimezone);