import { link } from './elements.js';
/**
 * Markdown segments to keep as is: replacing references inside them
 * would break already formatted links, code samples and urls.
 */
const protectedSegments = [
    /\[(?:[^[\]]|\[[^[\]]*])*]\((?:[^()\n]|\([^()\n]*\))*\)/, // [text](url)
    // a text and a label end before the next not nested `[`,
    // so a long run of brackets is not scanned to its end from every one of them
    /\[(?:[^[\]]|\[[^[\]]*])*]\[[^[\]]*]/, // [text][ref]
    // the lookahead makes the delimiter length atomic, without it a long run
    // of backticks makes the regex engine retry every possible delimiter length;
    // as in CommonMark, the info string of backticks has no backticks, and a block
    // without a closing fence lasts to the end, so the next fences don't scan it again
    /^[ \t]*(?=(?<fence>`{3,}|~{3,}))\k<fence>(?:(?<=`)[^`\n]*|(?<=~)[^\n]*)$[\s\S]*?(?:^[ \t]*\k<fence>[ \t]*\r?$|(?![\s\S]))/, // ```code block```
    // as in CommonMark, whole runs of backticks of the same length open and close a span;
    // only runs of up to 16 backticks open one, so at most 16 runs without a closing one
    // scan the rest of a line
    /(?<!`)(?=(?<code>`{1,16}(?!`)))\k<code>[^\n]*?(?<!`)\k<code>(?!`)/, // `code span`
    /https?:\/\/(?:[^\s()]|\([^\s()]*\))+/ // https://url
];
const userMention = /\B@(?<user>[a-z0-9](?:-?[a-z0-9/]){0,38})/;
const defaultIssuePattern = /[a-z0-9]+/;
/**
 * Escapes a string to use it as a part of a regex.
 * @param string
 * @returns Escaped string.
 */
function escapeRegExp(string) {
    return string.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
}
/**
 * Builds a regex which matches issue and user references
 * outside of the protected Markdown segments.
 * @param issuePrefixes
 * @param issuePattern
 * @returns Regex with `prefix`, `issue` and `user` groups.
 */
function referencesRegex(issuePrefixes = [], issuePattern = defaultIssuePattern) {
    const prefixes = issuePrefixes
        .map(prefix => (typeof prefix === 'string' ? escapeRegExp(prefix) : prefix.source))
        .join('|');
    const patterns = [
        ...protectedSegments.map(segment => segment.source),
        // without prefixes every word would be matched as an issue reference
        prefixes && `(?<prefix>${prefixes})(?<issue>${issuePattern.source})`,
        userMention.source
    ];
    return new RegExp(patterns.filter(Boolean).join('|'), 'gm');
}
/**
 * Creates a text formatter for the given options.
 *
 * Regexes from the options are inlined into a single multiline regex,
 * so their flags are ignored, `^` and `$` match line boundaries,
 * and they should not contain capturing groups, backreferences or named groups.
 * @param options - Formatter options.
 * @returns Formatter which replaces issue and user references in a text with links.
 */
export function createReferencesFormatter(options) {
    const regex = referencesRegex(options.issuePrefixes, options.issuePattern);
    return (text, context, references) => text.replace(regex, (match, ...args) => {
        const { prefix, issue, user } = args.at(-1);
        if (issue) {
            const issueUrl = options.formatIssueUrl(context, {
                prefix: prefix,
                issue
            });
            if (!issueUrl) {
                return match;
            }
            references?.push(match);
            return link(match, issueUrl);
        }
        if (user) {
            // TODO: investigate why this code exists.
            if (user.includes('/')) {
                return match;
            }
            const userUrl = options.formatUserUrl(context, user);
            if (!userUrl) {
                return match;
            }
            return link(match, userUrl);
        }
        return match;
    });
}
//# sourceMappingURL=data:application/json;base64,eyJ2ZXJzaW9uIjozLCJmaWxlIjoicmVmZXJlbmNlcy5qcyIsInNvdXJjZVJvb3QiOiIiLCJzb3VyY2VzIjpbIi4uL3NyYy9yZWZlcmVuY2VzLnRzIl0sIm5hbWVzIjpbXSwibWFwcGluZ3MiOiJBQUlBLE9BQU8sRUFBRSxJQUFJLEVBQUUsTUFBTSxlQUFlLENBQUE7QUFFcEM7OztHQUdHO0FBQ0gsTUFBTSxpQkFBaUIsR0FBRztJQUN4Qix3REFBd0QsRUFBRSxjQUFjO0lBQ3hFLHlEQUF5RDtJQUN6RCw2RUFBNkU7SUFDN0UscUNBQXFDLEVBQUUsY0FBYztJQUNyRCx5RUFBeUU7SUFDekUsNkVBQTZFO0lBQzdFLCtFQUErRTtJQUMvRSxtRkFBbUY7SUFDbkYsMkhBQTJILEVBQUUsbUJBQW1CO0lBQ2hKLHNGQUFzRjtJQUN0RixxRkFBcUY7SUFDckYsMEJBQTBCO0lBQzFCLG1FQUFtRSxFQUFFLGNBQWM7SUFDbkYsc0NBQXNDLENBQUMsY0FBYztDQUN0RCxDQUFBO0FBQ0QsTUFBTSxXQUFXLEdBQUcsMkNBQTJDLENBQUE7QUFDL0QsTUFBTSxtQkFBbUIsR0FBRyxXQUFXLENBQUE7QUF3Q3ZDOzs7O0dBSUc7QUFDSCxTQUFTLFlBQVksQ0FBQyxNQUFjO0lBQ2xDLE9BQU8sTUFBTSxDQUFDLE9BQU8sQ0FBQyxxQkFBcUIsRUFBRSxNQUFNLENBQUMsQ0FBQTtBQUN0RCxDQUFDO0FBRUQ7Ozs7OztHQU1HO0FBQ0gsU0FBUyxlQUFlLENBQ3RCLGFBQWEsR0FBd0IsRUFBRSxFQUN2QyxZQUFZLEdBQVcsbUJBQW1CO0lBRTFDLE1BQU0sUUFBUSxHQUFHLGFBQWE7U0FDM0IsR0FBRyxDQUFDLE1BQU0sQ0FBQyxFQUFFLENBQUMsQ0FBQyxPQUFPLE1BQU0sS0FBSyxRQUFRLENBQUMsQ0FBQyxDQUFDLFlBQVksQ0FBQyxNQUFNLENBQUMsQ0FBQyxDQUFDLENBQUMsTUFBTSxDQUFDLE1BQU0sQ0FBQyxDQUFDO1NBQ2xGLElBQUksQ0FBQyxHQUFHLENBQUMsQ0FBQTtJQUNaLE1BQU0sUUFBUSxHQUFHO1FBQ2YsR0FBRyxpQkFBaUIsQ0FBQyxHQUFHLENBQUMsT0FBTyxDQUFDLEVBQUUsQ0FBQyxPQUFPLENBQUMsTUFBTSxDQUFDO1FBQ25ELHFFQUFxRTtRQUNyRSxRQUFRLElBQUksYUFBYSxRQUFRLGFBQWEsWUFBWSxDQUFDLE1BQU0sR0FBRztRQUNwRSxXQUFXLENBQUMsTUFBTTtLQUNuQixDQUFBO0lBRUQsT0FBTyxJQUFJLE1BQU0sQ0FBQyxRQUFRLENBQUMsTUFBTSxDQUFDLE9BQU8sQ0FBQyxDQUFDLElBQUksQ0FBQyxHQUFHLENBQUMsRUFBRSxJQUFJLENBQUMsQ0FBQTtBQUM3RCxDQUFDO0FBRUQ7Ozs7Ozs7O0dBUUc7QUFDSCxNQUFNLFVBQVUseUJBQXlCLENBQ3ZDLE9BQTJDO0lBRTNDLE1BQU0sS0FBSyxHQUFHLGVBQWUsQ0FBQyxPQUFPLENBQUMsYUFBYSxFQUFFLE9BQU8sQ0FBQyxZQUFZLENBQUMsQ0FBQTtJQUUxRSxPQUFPLENBQUMsSUFBSSxFQUFFLE9BQU8sRUFBRSxVQUFVLEVBQUUsRUFBRSxDQUFDLElBQUksQ0FBQyxPQUFPLENBQUMsS0FBSyxFQUFFLENBQUMsS0FBSyxFQUFFLEdBQUcsSUFBSSxFQUFFLEVBQUU7UUFDM0UsTUFBTSxFQUNKLE1BQU0sRUFDTixLQUFLLEVBQ0wsSUFBSSxFQUNMLEdBQUcsSUFBSSxDQUFDLEVBQUUsQ0FBQyxDQUFDLENBQUMsQ0FBK0MsQ0FBQTtRQUU3RCxJQUFJLEtBQUssRUFBRSxDQUFDO1lBQ1YsTUFBTSxRQUFRLEdBQUcsT0FBTyxDQUFDLGNBQWMsQ0FBQyxPQUFPLEVBQUU7Z0JBQy9DLE1BQU0sRUFBRSxNQUFnQjtnQkFDeEIsS0FBSzthQUNOLENBQUMsQ0FBQTtZQUVGLElBQUksQ0FBQyxRQUFRLEVBQUUsQ0FBQztnQkFDZCxPQUFPLEtBQUssQ0FBQTtZQUNkLENBQUM7WUFFRCxVQUFVLEVBQUUsSUFBSSxDQUFDLEtBQUssQ0FBQyxDQUFBO1lBRXZCLE9BQU8sSUFBSSxDQUFDLEtBQUssRUFBRSxRQUFRLENBQUMsQ0FBQTtRQUM5QixDQUFDO1FBRUQsSUFBSSxJQUFJLEVBQUUsQ0FBQztZQUNULDBDQUEwQztZQUMxQyxJQUFJLElBQUksQ0FBQyxRQUFRLENBQUMsR0FBRyxDQUFDLEVBQUUsQ0FBQztnQkFDdkIsT0FBTyxLQUFLLENBQUE7WUFDZCxDQUFDO1lBRUQsTUFBTSxPQUFPLEdBQUcsT0FBTyxDQUFDLGFBQWEsQ0FBQyxPQUFPLEVBQUUsSUFBSSxDQUFDLENBQUE7WUFFcEQsSUFBSSxDQUFDLE9BQU8sRUFBRSxDQUFDO2dCQUNiLE9BQU8sS0FBSyxDQUFBO1lBQ2QsQ0FBQztZQUVELE9BQU8sSUFBSSxDQUFDLEtBQUssRUFBRSxPQUFPLENBQUMsQ0FBQTtRQUM3QixDQUFDO1FBRUQsT0FBTyxLQUFLLENBQUE7SUFDZCxDQUFDLENBQUMsQ0FBQTtBQUNKLENBQUMifQ==