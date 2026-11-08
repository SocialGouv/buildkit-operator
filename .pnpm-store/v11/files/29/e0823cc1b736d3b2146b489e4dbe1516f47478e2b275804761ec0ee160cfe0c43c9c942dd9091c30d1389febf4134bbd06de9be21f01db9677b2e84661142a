import { headerPartial, preamblePartial, commitPartial, footerPartial, template } from '@conventional-changelog/template';
import { isValid } from 'verkit';
import { formatDate, createComparator } from './utils.js';
const HASH_SHORT_LENGTH = 7;
const HEADER_MAX_LENGTH = 100;
/**
 * Default commit transform function.
 * @param commit
 * @param _context
 * @param options
 * @param options.formatDate - Date formatter function.
 * @returns Patch object for commit.
 */
export function defaultCommitTransform(commit, _context, options) {
    const { hash, header, committerDate } = commit;
    return {
        hash: typeof hash === 'string'
            ? hash.substring(0, HASH_SHORT_LENGTH)
            : hash,
        header: typeof header === 'string'
            ? header.substring(0, HEADER_MAX_LENGTH)
            : header,
        committerDate: committerDate
            ? options.formatDate(committerDate)
            : committerDate
    };
}
/**
 * Get final options object.
 * @param options
 * @returns Final options object.
 */
export function getFinalOptions(options) {
    const prefinalOptions = {
        groupBy: 'type',
        commitsSort: 'header',
        noteGroupsSort: 'title',
        notesSort: 'text',
        transform: defaultCommitTransform,
        generateOn: (commit) => typeof commit.version === 'string' && isValid(commit.version),
        finalizeContext: (context) => context,
        debug: () => { },
        formatDate,
        template,
        headerPartial,
        preamblePartial,
        commitPartial,
        footerPartial,
        reverse: false,
        ignoreReverted: true,
        doFlush: true,
        ...options
    };
    const finalOptions = {
        ...prefinalOptions,
        commitGroupsSort: createComparator(prefinalOptions.commitGroupsSort),
        commitsSort: createComparator(prefinalOptions.commitsSort),
        noteGroupsSort: createComparator(prefinalOptions.noteGroupsSort),
        notesSort: createComparator(prefinalOptions.notesSort)
    };
    return finalOptions;
}
/**
 * Get final context object.
 * @param context
 * @param options
 * @returns Final context object.
 */
export function getGenerateOnFunction(context, options) {
    const { generateOn } = options;
    if (typeof generateOn === 'string') {
        return (commit) => typeof commit[generateOn] !== 'undefined';
    }
    else if (typeof generateOn !== 'function') {
        return () => false;
    }
    return (keyCommit, commitsGroup) => generateOn(keyCommit, commitsGroup, context, options);
}
//# sourceMappingURL=data:application/json;base64,eyJ2ZXJzaW9uIjozLCJmaWxlIjoib3B0aW9ucy5qcyIsInNvdXJjZVJvb3QiOiIiLCJzb3VyY2VzIjpbIi4uL3NyYy9vcHRpb25zLnRzIl0sIm5hbWVzIjpbXSwibWFwcGluZ3MiOiJBQUFBLE9BQU8sRUFHTCxhQUFhLEVBQ2IsZUFBZSxFQUNmLGFBQWEsRUFDYixhQUFhLEVBQ2IsUUFBUSxFQUNULE1BQU0sa0NBQWtDLENBQUE7QUFDekMsT0FBTyxFQUFFLE9BQU8sRUFBRSxNQUFNLFFBQVEsQ0FBQTtBQUtoQyxPQUFPLEVBQ0wsVUFBVSxFQUNWLGdCQUFnQixFQUNqQixNQUFNLFlBQVksQ0FBQTtBQUVuQixNQUFNLGlCQUFpQixHQUFHLENBQUMsQ0FBQTtBQUMzQixNQUFNLGlCQUFpQixHQUFHLEdBQUcsQ0FBQTtBQUU3Qjs7Ozs7OztHQU9HO0FBQ0gsTUFBTSxVQUFVLHNCQUFzQixDQUNwQyxNQUFjLEVBQ2QsUUFBaUIsRUFDakIsT0FBaUQ7SUFFakQsTUFBTSxFQUNKLElBQUksRUFDSixNQUFNLEVBQ04sYUFBYSxFQUNkLEdBQUcsTUFBTSxDQUFBO0lBRVYsT0FBTztRQUNMLElBQUksRUFBRSxPQUFPLElBQUksS0FBSyxRQUFRO1lBQzVCLENBQUMsQ0FBQyxJQUFJLENBQUMsU0FBUyxDQUFDLENBQUMsRUFBRSxpQkFBaUIsQ0FBQztZQUN0QyxDQUFDLENBQUMsSUFBSTtRQUNSLE1BQU0sRUFBRSxPQUFPLE1BQU0sS0FBSyxRQUFRO1lBQ2hDLENBQUMsQ0FBQyxNQUFNLENBQUMsU0FBUyxDQUFDLENBQUMsRUFBRSxpQkFBaUIsQ0FBQztZQUN4QyxDQUFDLENBQUMsTUFBTTtRQUNWLGFBQWEsRUFBRSxhQUFhO1lBQzFCLENBQUMsQ0FBQyxPQUFPLENBQUMsVUFBVSxDQUFDLGFBQWEsQ0FBQztZQUNuQyxDQUFDLENBQUMsYUFBYTtLQUNDLENBQUE7QUFDdEIsQ0FBQztBQUVEOzs7O0dBSUc7QUFDSCxNQUFNLFVBQVUsZUFBZSxDQUM3QixPQUF3QjtJQUV4QixNQUFNLGVBQWUsR0FBRztRQUN0QixPQUFPLEVBQUUsTUFBZTtRQUN4QixXQUFXLEVBQUUsUUFBaUI7UUFDOUIsY0FBYyxFQUFFLE9BQWdCO1FBQ2hDLFNBQVMsRUFBRSxNQUFlO1FBQzFCLFNBQVMsRUFBRSxzQkFBc0I7UUFDakMsVUFBVSxFQUFFLENBQUMsTUFBYyxFQUFFLEVBQUUsQ0FBQyxPQUFPLE1BQU0sQ0FBQyxPQUFPLEtBQUssUUFBUSxJQUFJLE9BQU8sQ0FBQyxNQUFNLENBQUMsT0FBTyxDQUFDO1FBQzdGLGVBQWUsRUFBRSxDQUFDLE9BQXFDLEVBQUUsRUFBRSxDQUFDLE9BQU87UUFDbkUsS0FBSyxFQUFFLEdBQUcsRUFBRSxHQUFjLENBQUM7UUFDM0IsVUFBVTtRQUNWLFFBQVE7UUFDUixhQUFhO1FBQ2IsZUFBZTtRQUNmLGFBQWE7UUFDYixhQUFhO1FBQ2IsT0FBTyxFQUFFLEtBQUs7UUFDZCxjQUFjLEVBQUUsSUFBSTtRQUNwQixPQUFPLEVBQUUsSUFBSTtRQUNiLEdBQUcsT0FBTztLQUNYLENBQUE7SUFDRCxNQUFNLFlBQVksR0FBRztRQUNuQixHQUFHLGVBQWU7UUFDbEIsZ0JBQWdCLEVBQUUsZ0JBQWdCLENBQUMsZUFBZSxDQUFDLGdCQUFnQixDQUFDO1FBQ3BFLFdBQVcsRUFBRSxnQkFBZ0IsQ0FBQyxlQUFlLENBQUMsV0FBdUIsQ0FBQztRQUN0RSxjQUFjLEVBQUUsZ0JBQWdCLENBQUMsZUFBZSxDQUFDLGNBQWMsQ0FBQztRQUNoRSxTQUFTLEVBQUUsZ0JBQWdCLENBQUMsZUFBZSxDQUFDLFNBQVMsQ0FBQztLQUMvQixDQUFBO0lBRXpCLE9BQU8sWUFBWSxDQUFBO0FBQ3JCLENBQUM7QUFFRDs7Ozs7R0FLRztBQUNILE1BQU0sVUFBVSxxQkFBcUIsQ0FDbkMsT0FBcUMsRUFDckMsT0FBNkI7SUFFN0IsTUFBTSxFQUFFLFVBQVUsRUFBRSxHQUFHLE9BQU8sQ0FBQTtJQUU5QixJQUFJLE9BQU8sVUFBVSxLQUFLLFFBQVEsRUFBRSxDQUFDO1FBQ25DLE9BQU8sQ0FBQyxNQUFjLEVBQUUsRUFBRSxDQUFDLE9BQU8sTUFBTSxDQUFDLFVBQVUsQ0FBQyxLQUFLLFdBQVcsQ0FBQTtJQUN0RSxDQUFDO1NBQU0sSUFBSSxPQUFPLFVBQVUsS0FBSyxVQUFVLEVBQUUsQ0FBQztRQUM1QyxPQUFPLEdBQUcsRUFBRSxDQUFDLEtBQUssQ0FBQTtJQUNwQixDQUFDO0lBRUQsT0FBTyxDQUFDLFNBQWlCLEVBQUUsWUFBc0IsRUFBRSxFQUFFLENBQUMsVUFBVSxDQUFDLFNBQVMsRUFBRSxZQUFZLEVBQUUsT0FBTyxFQUFFLE9BQU8sQ0FBQyxDQUFBO0FBQzdHLENBQUMifQ==